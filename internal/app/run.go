package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/term"

	"k8c.io/agent-sandbox/internal/config"
	"k8c.io/agent-sandbox/internal/image"
	"k8c.io/agent-sandbox/internal/mounts"
	"k8c.io/agent-sandbox/internal/podman"
	"k8c.io/agent-sandbox/internal/project"
	"k8c.io/agent-sandbox/internal/sandbox"
	"k8c.io/agent-sandbox/internal/selfbin"
	"k8c.io/agent-sandbox/internal/trust"
)

// RunOptions are the main command's arguments.
type RunOptions struct {
	Name string
	Cmd  []string // after --; empty means config command or bash -l
}

// Run is the main entry: trust, ensure images, ensure running, exec. It
// returns the session's exit code.
func Run(ctx context.Context, o RunOptions) (int, error) {
	if err := project.ValidateName(o.Name); err != nil {
		return 1, err
	}
	e, err := resolve()
	if err != nil {
		return 1, err
	}
	state, err := e.checkTrust()
	if err != nil {
		return 1, err
	}
	p, proj := e.paths, e.proj

	// Config is always read from the host project root (spec 3.1).
	cfg, err := config.Load(p.GlobalConfig(), filepath.Join(proj.Root, trust.DirName, "config.toml"), config.Env{
		Home:        p.Home,
		ConfigHome:  p.ConfigHome,
		DataHome:    p.DataHome,
		XDGRuntime:  p.XDGRuntime,
		DataDir:     p.DataDir,
		ProjectRoot: proj.Root,
		PathDirs:    filepath.SplitList(os.Getenv("PATH")),
	})
	if err != nil {
		return 1, err
	}
	// Fail closed: never run with less isolation than the config asks for.
	if cfg.Network == "allowlist" {
		return 1, fmt.Errorf("network mode %q is not implemented in this build yet", cfg.Network)
	}

	if err := e.prepare(ctx); err != nil {
		return 1, err
	}
	hasSecrets, err := config.CheckSecrets(p.SecretsFile())
	if err != nil {
		return 1, err
	}
	im, err := e.ensureImages(ctx, state, image.BuildOptions{}, false)
	if err != nil {
		return 1, err
	}
	exe, err := executable()
	if err != nil {
		return 1, err
	}
	binPath, binHash, err := selfbin.Stage(exe, p.DataDir)
	if err != nil {
		return 1, fmt.Errorf("staging the agent-sandbox binary: %w", err)
	}
	plan, err := mounts.Build(mounts.Input{Root: proj.Root, HasGit: proj.HasGit, ID: proj.ID, Home: p.Home, Binary: binPath, Config: cfg})
	if err != nil {
		return 1, err
	}

	container := project.ContainerName(proj.ID, o.Name)
	envFile := ""
	if hasSecrets {
		envFile = p.SecretsFile()
	}
	runArgv := sandbox.RunArgs(sandbox.RunInput{
		Container: container,
		Labels:    sandbox.Labels(proj, o.Name, cfg.Git, im.imageID, binHash),
		Hostname:  project.Hostname(proj.Root),
		Resources: cfg.Resources,
		Mounts:    plan.Mounts,
		EnvFile:   envFile,
		Image:     im.tag,
		Command:   sandbox.InitCommand(cfg, os.Getuid()),
	})

	command := o.Cmd
	if len(command) == 0 {
		command = cfg.Command
	}
	if len(command) == 0 {
		command = []string{"bash", "-l"}
	}
	execArgv := sandbox.ExecArgs(sandbox.ExecInput{
		Container: container,
		UID:       os.Getuid(),
		GID:       os.Getgid(),
		Workdir:   project.MapWorkdir(e.cwd, proj.Root, proj.Root),
		TTY:       term.IsTerminal(int(os.Stdin.Fd())),
		Env:       sandbox.SessionEnv(cfg.Env, plan.Env),
		Cmd:       command,
	})

	sb := &sandbox.Sandbox{
		Podman:       e.pm,
		Stdio:        podman.Stdio{In: os.Stdin, Out: os.Stdout, Err: os.Stderr},
		Log:          os.Stderr,
		Container:    container,
		Lookup:       sandbox.Lookup(proj.ID, o.Name),
		RunArgv:      runArgv,
		LockDir:      p.RuntimeDir,
		Volumes:      plan.Volumes,
		VolumeLabels: sandbox.VolumeLabels(proj.ID),
		VolumeImage:  im.base,
		UID:          os.Getuid(),
		GID:          os.Getgid(),
		SeedDir:      p.HomeSeedDir(),
		TmpDir:       p.RuntimeDir,
		ImageID:      im.imageID,
		BinaryHash:   binHash,
	}
	return sb.Exec(ctx, execArgv)
}
