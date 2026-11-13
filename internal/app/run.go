package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/term"

	"k8c.io/agent-sandbox/internal/config"
	"k8c.io/agent-sandbox/internal/flock"
	"k8c.io/agent-sandbox/internal/fspath"
	"k8c.io/agent-sandbox/internal/hostgit"
	"k8c.io/agent-sandbox/internal/image"
	"k8c.io/agent-sandbox/internal/initd"
	"k8c.io/agent-sandbox/internal/mounts"
	"k8c.io/agent-sandbox/internal/podman"
	"k8c.io/agent-sandbox/internal/project"
	"k8c.io/agent-sandbox/internal/sandbox"
	"k8c.io/agent-sandbox/internal/scan"
	"k8c.io/agent-sandbox/internal/selfbin"
	"k8c.io/agent-sandbox/internal/trust"
	"k8c.io/agent-sandbox/internal/worktree"
)

// RunOptions are the main command's arguments.
type RunOptions struct {
	Name   string
	Cmd    []string // after --; empty means config command or bash -l
	Strict bool     // refuse to start while earlier scan findings exist
	GC     bool     // worktree mode: git gc on the host before starting
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
	if err := e.prepare(ctx); err != nil {
		return 1, err
	}

	// The mounted tree: the project root, or the sandbox's worktree.
	tree := proj.Root
	gitDir, _, _, err := hostgit.GitDirs(proj.Root)
	if err != nil {
		return 1, err
	}
	var gitMount *mounts.GitMount
	var gitEnv []string
	if cfg.Git == "worktree" {
		wt, err := e.ensureWorktree(ctx, o, cfg)
		if err != nil {
			return 1, err
		}
		tree, gitDir = wt.Path, wt.GitDir
		gitMount = &mounts.GitMount{GitDir: wt.GitDir, AdminDir: wt.AdminDir, ConfigWorktree: wt.ConfigWorktree}
		gitEnv = sandbox.GitSessionEnv(o.Name, cfg.GitUserName, cfg.GitUserEmail)
	}
	deny, err := e.denyList(cfg, tree)
	if err != nil {
		return 1, err
	}
	denyPresent, err := mounts.ResolveDeny(tree, deny)
	if err != nil {
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
	plan, err := mounts.Build(mounts.Input{Root: tree, Deny: denyPresent, Worktree: gitMount, ID: proj.ID, Home: p.Home, Binary: binPath, Config: cfg})
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
		Workdir:   workdir(e.cwd, proj.Root, tree),
		TTY:       term.IsTerminal(int(os.Stdin.Fd())),
		Env:       sandbox.SessionEnv(cfg.Env, sessionEnv(cfg, plan, gitEnv)),
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
		GitMode:      cfg.Git,
	}
	code, err := sb.Exec(ctx, execArgv)
	if err != nil {
		return code, err
	}
	e.postScan(ctx, container, scan.Input{
		Root:       tree,
		RootHadGit: proj.HasGit,
		GitDir:     gitDir,
		Deny:       deny,
	}, plan, statePath)
	return code, nil
}

// denyList is the built-in deny-list plus core.hooksPath targets and git
// config include files inside the project, and config readonly. A hooksPath directory that does not exist yet
// is created empty, so it can be overlaid: git on the host runs whatever the
// agent would put there.
func (e *env) denyList(cfg config.Config, tree string) ([]string, error) {
	gitCfg, err := hostgit.RepoConfig(tree, hostgit.Env{Home: e.paths.Home, ConfigHome: e.paths.ConfigHome})
	if err != nil {
		return nil, fmt.Errorf("reading git config: %w", err)
	}
	hooks := hostgit.HooksPathsInside(gitCfg, tree, e.paths.Home)
	for _, rel := range hooks {
		dir := filepath.Join(tree, rel)
		if fspath.Real(dir) != dir {
			return nil, fmt.Errorf("core.hooksPath %s is a symlink or lies under one; agent-sandbox cannot protect it read-only", dir)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating core.hooksPath %s: %w", dir, err)
		}
	}
	deny := mounts.DenyList(hooks, cfg.ReadOnly)
	includes, err := gitIncludes(hostgit.IncludesInside(gitCfg, tree), tree, deny)
	if err != nil {
		return nil, err
	}
	return mounts.DenyList(deny, includes), nil
}

// gitIncludes returns the git config include targets inside the project
// that no deny-list entry covers yet. A missing target is created empty, so
// it can be overlaid: host git reads whatever would appear there.
func gitIncludes(rels []string, tree string, deny []string) ([]string, error) {
	var out []string
	for _, rel := range rels {
		path := filepath.Join(tree, rel)
		if slices.ContainsFunc(deny, func(d string) bool { return fspath.Within(path, filepath.Join(tree, d)) }) {
			continue
		}
		if fspath.Real(path) != path {
			return nil, fmt.Errorf("git config include %s is a symlink or lies under one; agent-sandbox cannot protect it read-only", path)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("creating git config include %s: %w", path, err)
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		switch {
		case err == nil:
			if err := f.Close(); err != nil {
				return nil, err
			}
		case !errors.Is(err, fs.ErrExist):
			return nil, fmt.Errorf("creating git config include %s: %w", path, err)
		}
		out = append(out, rel)
	}
	return out, nil
}

// postScan runs the post-session scan (spec 7) and records and prints its
// findings. Failures are warnings: the session has already run.
func (e *env) postScan(ctx context.Context, container string, in scan.Input, plan mounts.Plan, statePath string) {
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
	in.Overlaid = overlaid
	current := scan.Scan(in)
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

// ensureWorktree creates or resumes the sandbox's worktree (spec 5.3) under
// a lock, so parallel invocations do not race on git worktree add.
func (e *env) ensureWorktree(ctx context.Context, o RunOptions, cfg config.Config) (worktree.Worktree, error) {
	unlock, err := flock.Lock(filepath.Join(e.paths.RuntimeDir, "worktree-"+e.proj.ID+"-"+o.Name+".lock"))
	if err != nil {
		return worktree.Worktree{}, err
	}
	defer unlock()
	git := hostgit.Runner{}
	if o.GC {
		// Packing the loose objects closes the window in which the agent
		// could overwrite them (spec 5.3, residual risk).
		if _, err := git.Run(ctx, e.proj.Root, "gc", "--quiet"); err != nil {
			return worktree.Worktree{}, fmt.Errorf("git gc: %w", err)
		}
	}
	wt, err := worktree.Ensure(ctx, git, worktree.Input{
		Root:      e.proj.Root,
		ProjectID: e.proj.ID,
		Name:      o.Name,
		DataDir:   e.paths.DataDir,
		Home:      e.paths.Home,
		Copy:      cfg.WorktreeCopy,
		Log:       os.Stderr,
	})
	if err != nil {
		return worktree.Worktree{}, err
	}
	if wt.Created {
		fmt.Fprintf(os.Stderr, "agent-sandbox: created worktree %s on branch %s\n", wt.Path, wt.Branch)
	}
	return wt, nil
}

// workdir maps cwd into the mounted tree, falling back to the tree's root
// when the directory does not exist there (a worktree of an older commit).
func workdir(cwd, root, tree string) string {
	w := project.MapWorkdir(cwd, root, tree)
	if fi, err := os.Stat(w); err != nil || !fi.IsDir() {
		return tree
	}
	return w
}

// sessionEnv is the wrapper's own session env: cache presets, git settings
// in worktree mode, and the proxy in allowlist mode.
func sessionEnv(cfg config.Config, plan mounts.Plan, gitEnv []string) []string {
	env := append(append([]string{}, plan.Env...), gitEnv...)
	if cfg.Network == "allowlist" {
		env = append(env, sandbox.ProxyEnv(initd.ProxyAddr)...)
	}
	return env
}
