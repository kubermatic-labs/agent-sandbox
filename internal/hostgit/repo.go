package hostgit

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"k8c.io/agent-sandbox/internal/fspath"
)

// GitDirs returns the repository's git dir and common dir for a worktree
// root whose .git is a directory or a "gitdir:" file. ok is false when
// root has no .git.
func GitDirs(root string) (gitDir, commonDir string, ok bool, err error) {
	dotgit := filepath.Join(root, ".git")
	fi, err := os.Lstat(dotgit)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	switch {
	case fi.IsDir():
		return dotgit, dotgit, true, nil
	case fi.Mode().IsRegular():
		gitDir, err = ReadGitFile(dotgit)
		if err != nil {
			return "", "", false, err
		}
	default:
		return "", "", false, fmt.Errorf("%s is neither a directory nor a regular file", dotgit)
	}
	commonDir = gitDir
	if b, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		cd := strings.TrimSpace(string(b))
		if !filepath.IsAbs(cd) {
			cd = filepath.Join(gitDir, cd)
		}
		commonDir = filepath.Clean(cd)
	}
	return gitDir, commonDir, true, nil
}

// ReadGitFile parses a .git file ("gitdir: <path>") and returns the
// absolute git dir it points to.
func ReadGitFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	line, _, _ := bytes.Cut(b, []byte("\n"))
	dir, ok := strings.CutPrefix(strings.TrimSpace(string(line)), "gitdir:")
	if !ok {
		return "", fmt.Errorf("%s: not a gitdir file", path)
	}
	dir = strings.TrimSpace(dir)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(filepath.Dir(path), dir)
	}
	return filepath.Clean(dir), nil
}

// Env locates the config files outside the repository.
type Env struct {
	Home       string
	ConfigHome string // XDG config base
}

// RepoConfig parses the config git would use in root: system, global
// (XDG and ~/.gitconfig) and the repository's own, with includes.
func RepoConfig(root string, env Env) (Config, error) {
	files := []string{"/etc/gitconfig", filepath.Join(env.ConfigHome, "git", "config"), filepath.Join(env.Home, ".gitconfig")}
	_, commonDir, ok, err := GitDirs(root)
	if err != nil {
		return Config{}, err
	}
	if ok {
		files = append(files, filepath.Join(commonDir, "config"))
	}
	return ParseFiles(env.Home, files...)
}

// HooksPathsInside returns every core.hooksPath value, from any config
// level, that points inside root, as root-relative paths. Git resolves a
// relative hooksPath against the worktree root. Over-approximating (all
// levels, not just the effective one) is deliberate.
func HooksPathsInside(cfg Config, root, home string) []string {
	var out []string
	for _, v := range cfg.All("core.hooksPath") {
		p := v
		switch {
		case p == "":
			continue
		case strings.HasPrefix(p, "~/"):
			p = filepath.Join(home, p[2:])
		case !filepath.IsAbs(p):
			p = filepath.Join(root, p)
		}
		p = filepath.Clean(p)
		if p == root || !fspath.Within(p, root) {
			continue
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, rel)
	}
	return out
}

// IncludesInside returns every include target, from any config level, that
// lies inside root (a real path), as root-relative paths. Host git reads
// these files, so an agent that could write one could set core.fsmonitor,
// core.pager or an alias that runs on the host. A target outside root that
// resolves into it through a symlink is returned as its resolved path.
func IncludesInside(cfg Config, root string) []string {
	var out []string
	for _, p := range cfg.Includes() {
		if !fspath.Within(p, root) {
			p = fspath.Real(p)
		}
		if p == root || !fspath.Within(p, root) {
			continue
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, rel)
	}
	return out
}
