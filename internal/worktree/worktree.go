// Package worktree creates and resumes the git worktree a sandbox works in
// when git = "worktree" (spec 5.3).
package worktree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"k8c.io/agent-sandbox/internal/fspath"
	"k8c.io/agent-sandbox/internal/hostgit"
)

// Worktree is a sandbox's worktree.
type Worktree struct {
	Path           string // the worktree, a real path
	GitDir         string // the main repository's .git
	AdminDir       string // GitDir/worktrees/<x>: HEAD, index, per-worktree refs
	Branch         string // asb/<name>
	ConfigWorktree bool   // AdminDir/config.worktree exists (extensions.worktreeConfig)
	Created        bool   // created by this call
}

// ProjectsDir holds one directory of worktrees per project, named by
// project ID.
func ProjectsDir(dataDir string) string { return filepath.Join(dataDir, "worktrees") }

// Path returns where a sandbox's worktree lives.
func Path(dataDir, projectID, name string) string {
	return filepath.Join(ProjectsDir(dataDir), projectID, name)
}

// Branch returns the branch a sandbox works on.
func Branch(name string) string { return "asb/" + name }

// Input describes the worktree to ensure.
type Input struct {
	Root      string // the main checkout, a real path
	ProjectID string
	Name      string
	DataDir   string
	Home      string
	Copy      []string  // worktree_copy entries, root-relative
	Log       io.Writer // warnings
}

// Ensure returns the sandbox's worktree, creating it on first use. A
// worktree directory git does not know, or a registration without its
// directory, is an error: nothing is ever deleted.
func Ensure(ctx context.Context, git hostgit.Runner, in Input) (Worktree, error) {
	gitDir := filepath.Join(in.Root, ".git")
	if fi, err := os.Lstat(gitDir); err != nil || !fi.IsDir() {
		return Worktree{}, fmt.Errorf("worktree mode needs %s to be the main checkout with a .git directory", in.Root)
	}
	cfg, err := hostgit.ParseFiles(in.Home, filepath.Join(gitDir, "config"))
	if err != nil {
		return Worktree{}, err
	}
	if v, _ := cfg.Get("extensions.refStorage"); strings.EqualFold(v, "reftable") {
		return Worktree{}, errors.New("worktree mode does not support reftable repositories: refs live in .git/reftable/ and cannot be opened per branch; use git = \"ro\"")
	}

	w := Worktree{Branch: Branch(in.Name), GitDir: gitDir}
	path := Path(in.DataDir, in.ProjectID, in.Name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Worktree{}, err
	}
	path = fspath.Real(path)

	registered, err := registeredWorktrees(ctx, git, in.Root)
	if err != nil {
		return Worktree{}, err
	}
	_, statErr := os.Lstat(path)
	exists := statErr == nil
	switch {
	case exists && !registered[path]:
		return Worktree{}, fmt.Errorf("%s exists but git does not know it as a worktree; inspect it, then remove it or run 'git worktree prune' in %s", path, in.Root)
	case !exists && registered[path]:
		return Worktree{}, fmt.Errorf("git knows a worktree at %s but the directory is missing; run 'git worktree prune' in %s", path, in.Root)
	case !exists:
		if err := create(ctx, git, in.Root, path, w.Branch); err != nil {
			return Worktree{}, err
		}
		w.Created = true
		copyEntries(in.Root, path, in.Copy, in.Log)
	}
	w.Path = path

	admin, err := hostgit.ReadGitFile(filepath.Join(path, ".git"))
	if err != nil {
		return Worktree{}, err
	}
	if !fspath.Within(admin, filepath.Join(gitDir, "worktrees")) || admin == filepath.Join(gitDir, "worktrees") {
		return Worktree{}, fmt.Errorf("%s/.git points to %s, outside %s/worktrees", path, admin, gitDir)
	}
	w.AdminDir = admin

	// Mount points for the openings (spec 5.3) must exist before mounting: a
	// read-only overlay can only cover an existing path, and without
	// objects/info the agent could create objects/info/alternates.
	for _, d := range []string{"refs/heads/asb", "logs/refs/heads/asb", "objects/pack", "objects/info"} {
		if err := os.MkdirAll(filepath.Join(gitDir, d), 0o755); err != nil {
			return Worktree{}, fmt.Errorf("preparing %s: %w", d, err)
		}
	}
	if v, _ := cfg.Get("extensions.worktreeConfig"); strings.EqualFold(v, "true") {
		cw := filepath.Join(admin, "config.worktree")
		f, err := os.OpenFile(cw, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return Worktree{}, err
		}
		if err := f.Close(); err != nil {
			return Worktree{}, err
		}
		w.ConfigWorktree = true
	} else if _, err := os.Stat(filepath.Join(admin, "config.worktree")); err == nil {
		w.ConfigWorktree = true
	}
	return w, nil
}

// registeredWorktrees returns the worktree paths git knows, as real paths.
func registeredWorktrees(ctx context.Context, git hostgit.Runner, root string) (map[string]bool, error) {
	out, err := git.Run(ctx, root, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	paths := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			paths[fspath.Real(p)] = true
		}
	}
	return paths, nil
}

func create(ctx context.Context, git hostgit.Runner, root, path, branch string) error {
	_, err := git.Run(ctx, root, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	var ee *hostgit.ExitError
	switch {
	case err == nil:
		_, err = git.Run(ctx, root, "worktree", "add", path, branch)
	case errors.As(err, &ee) && ee.Code == 1:
		_, err = git.Run(ctx, root, "worktree", "add", "-b", branch, path, "HEAD")
	}
	if err != nil {
		return fmt.Errorf("creating worktree: %w", err)
	}
	return nil
}

// copyEntries copies worktree_copy entries from root into a new worktree.
// Missing entries and anything reached through a symlink are skipped with
// a warning; symlinks inside entries are skipped too. The destination is a
// checkout of the agent's branch, which may hold symlinks the agent
// committed, so writes never go through a symlink there either: a
// destination that is or lies under one is refused, and all writes go
// through an os.Root at the worktree, which cannot be escaped.
func copyEntries(root, wt string, entries []string, log io.Writer) {
	r, err := os.OpenRoot(wt)
	if err != nil {
		fmt.Fprintf(log, "agent-sandbox: worktree_copy: %v, nothing copied\n", err)
		return
	}
	defer func() { _ = r.Close() }()
	for _, rel := range entries {
		src := filepath.Join(root, rel)
		if _, err := os.Lstat(src); err != nil {
			fmt.Fprintf(log, "agent-sandbox: worktree_copy: %s: not found, skipped\n", rel)
			continue
		}
		if fspath.Real(src) != src {
			fmt.Fprintf(log, "agent-sandbox: worktree_copy: %s is a symlink or lies under one, skipped\n", rel)
			continue
		}
		if err := copyTree(src, wt, r, filepath.Clean(rel), log); err != nil {
			fmt.Fprintf(log, "agent-sandbox: worktree_copy: %s: %v\n", rel, err)
		}
	}
}

// copyTree copies src to dst, a path relative to the worktree wt (a real
// path) that r is opened at.
func copyTree(src, wt string, r *os.Root, dst string, log io.Writer) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if abs := filepath.Join(wt, target); fspath.Real(abs) != abs {
			return fmt.Errorf("%s in the worktree is a symlink or lies under one (from the branch); not copied", target)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return r.MkdirAll(target, info.Mode().Perm())
		case info.Mode().IsRegular():
			if err := r.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return r.WriteFile(target, b, info.Mode().Perm())
		default:
			fmt.Fprintf(log, "agent-sandbox: worktree_copy: %s is not a regular file, skipped\n", p)
			return nil
		}
	})
}
