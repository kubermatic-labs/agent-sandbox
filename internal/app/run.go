package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/term"

	"k8c.io/agent-sandbox/internal/config"
	"k8c.io/agent-sandbox/internal/fspath"
	"k8c.io/agent-sandbox/internal/hostgit"
	"k8c.io/agent-sandbox/internal/image"
	"k8c.io/agent-sandbox/internal/mounts"
	"k8c.io/agent-sandbox/internal/podman"
	"k8c.io/agent-sandbox/internal/project"
	"k8c.io/agent-sandbox/internal/sandbox"
	"k8c.io/agent-sandbox/internal/scan"
	"k8c.io/agent-sandbox/internal/selfbin"
	"k8c.io/agent-sandbox/internal/trust"
)

// RunOptions are the main command's arguments.
type RunOptions struct {
	Name   string
	Cmd    []string // after --; empty means config command or bash -l
	Strict bool     // refuse to start while earlier scan findings exist
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
	statePath := scan.StatePath(p.DataDir, proj.ID)
	if o.Strict || cfg.Strict {
		recorded, err := scan.Load(statePath)
		if err != nil {
			return 1, err
		}
		if open := scan.StillPresent(recorded); len(open) > 0 {
			fmt.Fprint(os.Stderr, scan.Warning(open, statePath))
			return 1, fmt.Errorf("refusing to start (strict): %d earlier finding(s) not resolved", len(open))
		}
	}
	deny, err := e.denyList(cfg)
	if err != nil {
		return 1, err
	}
	denyPresent, err := mounts.ResolveDeny(proj.Root, deny)
	if err != nil {
		return 1, err
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
	plan, err := mounts.Build(mounts.Input{Root: proj.Root, Deny: denyPresent, ID: proj.ID, Home: p.Home, Binary: binPath, Config: cfg})
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
	code, err := sb.Exec(ctx, execArgv)
	if err != nil {
		return code, err
	}
	e.postScan(ctx, container, deny, plan, statePath)
	return code, nil
}

// denyList is the built-in deny-list plus core.hooksPath targets inside the
// project and config readonly. A hooksPath directory that does not exist yet
// is created empty, so it can be overlaid: git on the host runs whatever the
// agent would put there.
func (e *env) denyList(cfg config.Config) ([]string, error) {
	gitCfg, err := hostgit.RepoConfig(e.proj.Root, hostgit.Env{Home: e.paths.Home, ConfigHome: e.paths.ConfigHome})
	if err != nil {
		return nil, fmt.Errorf("reading git config: %w", err)
	}
	hooks := hostgit.HooksPathsInside(gitCfg, e.proj.Root, e.paths.Home)
	for _, rel := range hooks {
		dir := filepath.Join(e.proj.Root, rel)
		if fspath.Real(dir) != dir {
			return nil, fmt.Errorf("core.hooksPath %s is a symlink or lies under one; agent-sandbox cannot protect it read-only", dir)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating core.hooksPath %s: %w", dir, err)
		}
	}
	return mounts.DenyList(hooks, cfg.ReadOnly), nil
}

// postScan runs the post-session scan (spec 7) and records and prints its
// findings. Failures are warnings: the session has already run.
func (e *env) postScan(ctx context.Context, container string, deny []string, plan mounts.Plan, statePath string) {
	overlaid := map[string]bool{}
	dsts, err := e.pm.MountDestinations(ctx, container)
	if err != nil { // the sandbox is gone; fall back to this run's plan
		for _, m := range plan.Mounts {
			dsts = append(dsts, m.Dst)
		}
	}
	for _, d := range dsts {
		overlaid[d] = true
	}
	gitDir, _, _, _ := hostgit.GitDirs(e.proj.Root)
	current := scan.Scan(scan.Input{
		Root:       e.proj.Root,
		RootHadGit: e.proj.HasGit,
		GitDir:     gitDir,
		Deny:       deny,
		Overlaid:   overlaid,
	})
	recorded, err := scan.Load(statePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-sandbox: reading %s: %v\n", statePath, err)
	}
	all := scan.Merge(recorded, current)
	if err := scan.Save(statePath, all); err != nil {
		fmt.Fprintf(os.Stderr, "agent-sandbox: recording scan findings: %v\n", err)
	}
	if len(all) > 0 {
		fmt.Fprint(os.Stderr, scan.Warning(all, statePath))
	}
}
