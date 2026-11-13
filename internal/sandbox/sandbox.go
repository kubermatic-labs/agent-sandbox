package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"k8c.io/agent-sandbox/internal/flock"
	"k8c.io/agent-sandbox/internal/initd"
	"k8c.io/agent-sandbox/internal/mounts"
	"k8c.io/agent-sandbox/internal/podman"
)

// Sandbox is one named sandbox of one project, fully resolved.
type Sandbox struct {
	Podman podman.Client
	Stdio  podman.Stdio // the session's stdio
	Log    io.Writer    // wrapper messages and logs of failed containers

	Container string            // container name
	Lookup    map[string]string // labels identifying this sandbox
	RunArgv   []string          // podman run argv (RunArgs)
	LockDir   string            // holds <container>.lock

	Volumes      []mounts.Volume
	VolumeLabels map[string]string
	VolumeImage  string // image for the one-shot volume init container
	UID, GID     int
	SeedDir      string // home seed; ignored if missing
	TmpDir       string // where the seed copy is staged

	ReadyTimeout time.Duration // default 10s
	ReadyPoll    time.Duration // default 100ms
}

// Exec ensures the container runs and execs execArgv (ExecArgs) in it,
// returning the session's exit code. If the container vanished between the
// check and the exec (it went idle), it is started again and the exec
// retried exactly once (spec 2.4).
func (s *Sandbox) Exec(ctx context.Context, execArgv []string) (int, error) {
	if err := s.ensureLocked(ctx); err != nil {
		return 1, err
	}
	code, err := s.Podman.R.Stream(ctx, s.Stdio, execArgv...)
	if err != nil || code != 125 {
		return code, err
	}
	// 125 is podman's own failure code. Retry only if the container is gone.
	running, err := s.running(ctx)
	if err != nil || running {
		return code, nil
	}
	fmt.Fprintln(s.Log, "agent-sandbox: the sandbox stopped before the session started; starting it again")
	if err := s.ensureLocked(ctx); err != nil {
		return 1, err
	}
	return s.Podman.R.Stream(ctx, s.Stdio, execArgv...)
}

func (s *Sandbox) ensureLocked(ctx context.Context) error {
	unlock, err := flock.Lock(filepath.Join(s.LockDir, s.Container+".lock"))
	if err != nil {
		return err
	}
	defer unlock()
	return s.ensure(ctx)
}

// ensure makes sure the container is running. Must hold the lock.
func (s *Sandbox) ensure(ctx context.Context) error {
	cs, err := s.Podman.ListContainers(ctx, s.Lookup)
	if err != nil {
		return err
	}
	for _, c := range cs {
		if c.State == "running" {
			return nil
		}
	}
	for _, c := range cs {
		if c.ExitCode != 0 {
			fmt.Fprintf(s.Log, "agent-sandbox: the previous sandbox exited with code %d; its logs:\n", c.ExitCode)
			if err := s.Podman.Logs(ctx, c.ID, s.Log); err != nil {
				fmt.Fprintf(s.Log, "agent-sandbox: reading logs: %v\n", err)
			}
		}
		if err := s.Podman.Remove(ctx, c.ID); err != nil {
			return err
		}
	}
	if err := s.ensureVolumes(ctx); err != nil {
		return err
	}
	if _, err := s.Podman.R.Output(ctx, s.RunArgv...); err != nil {
		return fmt.Errorf("starting the sandbox: %w", err)
	}
	return s.waitReady(ctx)
}

// waitReady polls for _init's ready file. If the container exits first or
// the file does not appear in time, it prints the logs and removes the
// container: a sandbox whose setup failed is never used.
func (s *Sandbox) waitReady(ctx context.Context) error {
	timeout, poll := s.ReadyTimeout, s.ReadyPoll
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	if poll == 0 {
		poll = 100 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	for {
		_, err := s.Podman.R.Output(ctx, "exec", s.Container, "test", "-e", initd.ReadyFile)
		if err == nil {
			return nil
		}
		var ee *podman.ExitError
		if !errors.As(err, &ee) {
			return err
		}
		if ee.Code != 1 { // not just "no file yet": is the container still there?
			running, err := s.running(ctx)
			if err != nil {
				return err
			}
			if !running {
				return s.failStart(ctx, "the sandbox exited during startup")
			}
		}
		if time.Now().After(deadline) {
			return s.failStart(ctx, fmt.Sprintf("the sandbox did not become ready within %v", timeout))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (s *Sandbox) failStart(ctx context.Context, msg string) error {
	fmt.Fprintf(s.Log, "agent-sandbox: %s; its logs:\n", msg)
	if err := s.Podman.Logs(ctx, s.Container, s.Log); err != nil {
		fmt.Fprintf(s.Log, "agent-sandbox: reading logs: %v\n", err)
	}
	if err := s.Podman.Remove(ctx, s.Container); err != nil {
		fmt.Fprintf(s.Log, "agent-sandbox: removing the failed sandbox: %v\n", err)
	}
	return errors.New(msg)
}

func (s *Sandbox) running(ctx context.Context) (bool, error) {
	cs, err := s.Podman.ListContainers(ctx, s.Lookup)
	if err != nil {
		return false, err
	}
	for _, c := range cs {
		if c.State == "running" {
			return true, nil
		}
	}
	return false, nil
}

// ensureVolumes creates missing volumes and gives them to the session user.
// A volume whose init fails is removed again, so it is retried next time.
func (s *Sandbox) ensureVolumes(ctx context.Context) error {
	for _, v := range s.Volumes {
		ok, err := s.Podman.VolumeExists(ctx, v.Name)
		if err != nil {
			return err
		}
		if ok {
			continue
		}
		if err := s.Podman.VolumeCreate(ctx, v.Name, s.VolumeLabels); err != nil {
			return err
		}
		if err := s.initVolume(ctx, v); err != nil {
			_ = s.Podman.VolumeRemove(ctx, v.Name)
			return fmt.Errorf("initializing volume %s: %w", v.Name, err)
		}
	}
	return nil
}

func (s *Sandbox) initVolume(ctx context.Context, v mounts.Volume) error {
	seed := ""
	if v.Home {
		dir, cleanup, err := stageSeed(s.SeedDir, s.TmpDir)
		if err != nil {
			return err
		}
		defer cleanup()
		seed = dir
	}
	_, err := s.Podman.R.Output(ctx, VolumeInitArgs(v.Name, s.UID, s.GID, s.VolumeImage, seed)...)
	return err
}

// stageSeed copies the home seed into a private temp dir, which the init
// container mounts with :Z. The real config dir is never relabeled. It
// returns "" if there is no seed.
func stageSeed(seedDir, tmpDir string) (string, func(), error) {
	noop := func() {}
	if seedDir == "" {
		return "", noop, nil
	}
	if _, err := os.Stat(seedDir); errors.Is(err, fs.ErrNotExist) {
		return "", noop, nil
	} else if err != nil {
		return "", noop, err
	}
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return "", noop, err
	}
	dir, err := os.MkdirTemp(tmpDir, "seed-")
	if err != nil {
		return "", noop, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := os.CopyFS(dir, os.DirFS(seedDir)); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("copying home seed %s: %w", seedDir, err)
	}
	return dir, cleanup, nil
}
