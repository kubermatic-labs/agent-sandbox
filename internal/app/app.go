// Package app wires the packages together into the CLI's main flow (spec 2.4).
package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/term"

	"k8c.io/agent-sandbox/internal/config"
	"k8c.io/agent-sandbox/internal/fspath"
	"k8c.io/agent-sandbox/internal/image"
	"k8c.io/agent-sandbox/internal/mounts"
	"k8c.io/agent-sandbox/internal/paths"
	"k8c.io/agent-sandbox/internal/podman"
	"k8c.io/agent-sandbox/internal/project"
	"k8c.io/agent-sandbox/internal/sandbox"
	"k8c.io/agent-sandbox/internal/selfbin"
)

// RunOptions are the main command's arguments.
type RunOptions struct {
	Name string
	Cmd  []string // after --; empty means config command or bash -l
}

// Run is the main entry: ensure image, ensure running, exec. It returns the
// session's exit code.
func Run(ctx context.Context, o RunOptions) (int, error) {
	if err := project.ValidateName(o.Name); err != nil {
		return 1, err
	}
	p, err := paths.FromEnv(os.Getenv)
	if err != nil {
		return 1, err
	}
	wd, err := os.Getwd()
	if err != nil {
		return 1, err
	}
	cwd, err := filepath.EvalSymlinks(wd)
	if err != nil {
		return 1, err
	}
	proj := project.Resolve(cwd)
	protected := []string{p.ConfigDir, p.DataDir, p.XDGRuntime, filepath.Join(p.Home, ".ssh"), filepath.Join(p.Home, ".gnupg")}
	if err := project.CheckRoot(proj.Root, fspath.Real(p.Home), protected); err != nil {
		return 1, err
	}

	// Config is always read from the host project root (spec 3.1).
	cfg, err := config.Load(p.GlobalConfig(), filepath.Join(proj.Root, ".agent-sandbox", "config.toml"), config.Env{
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
	if cfg.Network != "open" {
		return 1, fmt.Errorf("network mode %q is not implemented in this build yet; set network = \"open\" in %s to use it", cfg.Network, p.GlobalConfig())
	}

	pm := podman.Client{R: podman.CLI{}}
	if err := checkPodman(ctx, pm); err != nil {
		return 1, err
	}
	hasSecrets, err := config.CheckSecrets(p.SecretsFile())
	if err != nil {
		return 1, err
	}
	if err := os.MkdirAll(p.RuntimeDir, 0o700); err != nil {
		return 1, err
	}

	tag, err := image.EnsureBase(ctx, pm, p.BaseOverrideDir(), p.RuntimeDir, image.BuildOptions{}, false, os.Stderr)
	if err != nil {
		return 1, err
	}
	imageID, err := pm.ImageID(ctx, tag)
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
		Labels:    sandbox.Labels(proj, o.Name, cfg.Git, imageID, binHash),
		Hostname:  project.Hostname(proj.Root),
		Resources: cfg.Resources,
		Mounts:    plan.Mounts,
		EnvFile:   envFile,
		Image:     tag,
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
		Workdir:   project.MapWorkdir(cwd, proj.Root, proj.Root),
		TTY:       term.IsTerminal(int(os.Stdin.Fd())),
		Env:       sandbox.SessionEnv(cfg.Env, plan.Env),
		Cmd:       command,
	})

	sb := &sandbox.Sandbox{
		Podman:       pm,
		Stdio:        podman.Stdio{In: os.Stdin, Out: os.Stdout, Err: os.Stderr},
		Log:          os.Stderr,
		Container:    container,
		Lookup:       sandbox.Lookup(proj.ID, o.Name),
		RunArgv:      runArgv,
		LockDir:      p.RuntimeDir,
		Volumes:      plan.Volumes,
		VolumeLabels: sandbox.VolumeLabels(proj.ID),
		VolumeImage:  tag,
		UID:          os.Getuid(),
		GID:          os.Getgid(),
		SeedDir:      p.HomeSeedDir(),
		TmpDir:       p.RuntimeDir,
	}
	return sb.Exec(ctx, execArgv)
}

// Build force-builds the base image.
func Build(ctx context.Context, opts image.BuildOptions) error {
	p, err := paths.FromEnv(os.Getenv)
	if err != nil {
		return err
	}
	pm := podman.Client{R: podman.CLI{}}
	if err := checkPodman(ctx, pm); err != nil {
		return err
	}
	if err := os.MkdirAll(p.RuntimeDir, 0o700); err != nil {
		return err
	}
	_, err = image.EnsureBase(ctx, pm, p.BaseOverrideDir(), p.RuntimeDir, opts, true, os.Stderr)
	return err
}

func checkPodman(ctx context.Context, pm podman.Client) error {
	v, err := pm.Version(ctx)
	if err != nil {
		return fmt.Errorf("podman is missing or not working: %w", err)
	}
	return podman.CheckVersion(v)
}

// executable is the running binary with symlinks (such as asb) resolved.
func executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}
