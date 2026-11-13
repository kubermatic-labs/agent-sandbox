// Package app wires the packages together into the CLI's commands (spec 2.3, 2.4).
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/term"

	"k8c.io/agent-sandbox/internal/config"
	"k8c.io/agent-sandbox/internal/fspath"
	"k8c.io/agent-sandbox/internal/image"
	"k8c.io/agent-sandbox/internal/paths"
	"k8c.io/agent-sandbox/internal/podman"
	"k8c.io/agent-sandbox/internal/project"
	"k8c.io/agent-sandbox/internal/trust"
)

// env is the resolved host context shared by the commands.
type env struct {
	paths paths.Paths
	cwd   string
	proj  project.Project
	pm    podman.Client
}

// resolve finds the paths and the project for the current directory.
func resolve() (*env, error) {
	p, err := paths.FromEnv(os.Getenv)
	if err != nil {
		return nil, err
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	cwd, err := filepath.EvalSymlinks(wd)
	if err != nil {
		return nil, err
	}
	proj := project.Resolve(cwd)
	protected := []string{p.ConfigDir, p.DataDir, p.XDGRuntime, filepath.Join(p.Home, ".ssh"), filepath.Join(p.Home, ".gnupg")}
	if err := project.CheckRoot(proj.Root, fspath.Real(p.Home), protected); err != nil {
		return nil, err
	}
	return &env{paths: p, cwd: cwd, proj: proj, pm: podman.Client{R: podman.CLI{}}}, nil
}

// prepare checks podman and creates the runtime dir.
func (e *env) prepare(ctx context.Context) error {
	v, err := e.pm.Version(ctx)
	if err != nil {
		return fmt.Errorf("podman is missing or not working: %w", err)
	}
	if err := podman.CheckVersion(v); err != nil {
		return err
	}
	return os.MkdirAll(e.paths.RuntimeDir, 0o700)
}

func (e *env) trustStore() trust.Store {
	return trust.NewStore(e.paths.DataDir, e.proj.ID)
}

// errUntrusted is returned when approval is needed but nobody can give it.
var errUntrusted = errors.New("untrusted sandbox config; run 'agent-sandbox trust'")

// checkTrust gates .agent-sandbox/ (spec 3.4): trusted or absent passes; an
// untrusted one is shown and prompted for on a terminal, refused otherwise.
func (e *env) checkTrust() (trust.State, error) {
	c, err := trust.Evaluate(e.proj.Root, e.trustStore())
	if err != nil {
		return 0, err
	}
	defer c.Discard()
	if c.State != trust.Untrusted {
		return c.State, nil
	}
	if !interactive() {
		return 0, errUntrusted
	}
	ok, err := trust.Prompt(c, e.proj.Root, os.Stdin, os.Stderr)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, errors.New("sandbox config not trusted")
	}
	if err := c.Approve(); err != nil {
		return 0, fmt.Errorf("recording approval: %w", err)
	}
	return trust.Trusted, nil
}

// projectConfig is the project config file to load for a checkTrust state:
// the approved snapshot's copy when trusted, none otherwise. An Implicit
// project had no .agent-sandbox/ when checked, so a config.toml that
// appears later is unapproved and ignored.
func (e *env) projectConfig(state trust.State) string {
	if state != trust.Trusted {
		return ""
	}
	return filepath.Join(e.trustStore().SnapshotDir(), "config.toml")
}

// loadConfig loads the global config and, if the project is trusted, the
// approved project config.
func (e *env) loadConfig(state trust.State) (config.Config, error) {
	p := e.paths
	return config.Load(p.GlobalConfig(), e.projectConfig(state), config.Env{
		Home:        p.Home,
		ConfigHome:  p.ConfigHome,
		DataHome:    p.DataHome,
		XDGRuntime:  p.XDGRuntime,
		DataDir:     p.DataDir,
		ProjectRoot: e.proj.Root,
		PathDirs:    filepath.SplitList(os.Getenv("PATH")),
	})
}

// interactive reports whether a person can answer a prompt.
func interactive() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

// images are the images a sandbox of this project runs.
type images struct {
	base    string // base image tag, also used for volume init
	tag     string // the sandbox image: project image if any, else base
	imageID string
}

// ensureImages builds what is missing (or everything with force), on the
// base image base (config key base). The
// project image is built from the trusted snapshot, never the live dir.
func (e *env) ensureImages(ctx context.Context, state trust.State, base string, opts image.BuildOptions, force bool) (images, error) {
	var im images
	var err error
	if im.base, err = image.EnsureBase(ctx, e.pm, base, e.paths.BaseOverrideDir(), e.paths.RuntimeDir, opts, force, os.Stderr); err != nil {
		return im, err
	}
	baseID, err := e.pm.ImageID(ctx, im.base)
	if err != nil {
		return im, err
	}
	im.tag, im.imageID = im.base, baseID
	if state != trust.Trusted {
		return im, nil
	}
	snap := e.trustStore().SnapshotDir()
	if _, err := os.Stat(filepath.Join(snap, "Containerfile")); err != nil {
		return im, nil
	}
	im.tag, err = image.EnsureProject(ctx, e.pm, image.ProjectInput{
		ProjectID:   e.proj.ID,
		ContextDir:  snap,
		BaseTag:     im.base,
		BaseImageID: baseID,
		WorkDir:     e.paths.RuntimeDir,
	}, opts, force, os.Stderr)
	if err != nil {
		return im, err
	}
	im.imageID, err = e.pm.ImageID(ctx, im.tag)
	return im, err
}

// executable is the running binary with symlinks (such as asb) resolved.
func executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}
