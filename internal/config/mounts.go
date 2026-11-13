package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"k8c.io/agent-sandbox/internal/fspath"
)

// Security-critical: these rules decide which host paths a project config can
// expose to the agent (spec 3.1).

var deniedSystemDirs = []string{"/etc", "/usr", "/boot", "/proc", "/sys", "/dev", "/run", "/var/run"}

// reservedDsts are container paths agent-sandbox itself mounts or relies on.
var reservedDsts = []string{"/usr/local/bin/agent-sandbox", "/cache", "/run/asb", "/proc", "/sys", "/dev"}

// resolveMount validates one mounts entry and returns it with src resolved
// (~ expanded, symlinks resolved) and dst expanded.
func resolveMount(m Mount, env Env) (Mount, error) {
	if m.Src == "" || m.Dst == "" {
		return Mount{}, errors.New("src and dst are required")
	}
	for _, p := range []string{m.Src, m.Dst} {
		if strings.ContainsAny(p, ":,") {
			return Mount{}, fmt.Errorf("%q: ':' and ',' are not allowed in mount paths", p)
		}
	}
	src, err := expandHome(m.Src, env.Home)
	if err != nil {
		return Mount{}, fmt.Errorf("src %w", err)
	}
	real, err := filepath.EvalSymlinks(src)
	if err != nil {
		return Mount{}, fmt.Errorf("src %q: %w", m.Src, err)
	}
	if err := checkSrc(real, !m.RO, env); err != nil {
		return Mount{}, fmt.Errorf("src %q (resolves to %s): %w", m.Src, real, err)
	}
	dst, err := expandHome(m.Dst, env.Home)
	if err != nil {
		return Mount{}, fmt.Errorf("dst %w", err)
	}
	if err := checkDst(dst, env); err != nil {
		return Mount{}, fmt.Errorf("dst %q: %w", m.Dst, err)
	}
	return Mount{Src: real, Dst: dst, RO: m.RO}, nil
}

func expandHome(p, home string) (string, error) {
	switch {
	case p == "~":
		p = home
	case strings.HasPrefix(p, "~/"):
		p = filepath.Join(home, p[2:])
	case strings.HasPrefix(p, "~"):
		return "", fmt.Errorf("%q: only ~ and ~/ are supported", p)
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%q must be absolute or start with ~/", p)
	}
	return filepath.Clean(p), nil
}

func checkSrc(real string, rw bool, env Env) error {
	home := fspath.Real(env.Home)
	if real == "/" || real == home {
		return errors.New("mounting / or $HOME is not allowed")
	}
	denied := append(slices.Clone(deniedSystemDirs),
		env.XDGRuntime,
		filepath.Join(env.Home, ".ssh"),
		filepath.Join(env.Home, ".gnupg"),
		filepath.Join(env.Home, ".config"),
		env.ConfigHome,
		filepath.Join(env.Home, ".local", "share", "containers"),
		filepath.Join(env.DataHome, "containers"),
		env.DataDir,
	)
	for _, d := range denied {
		if !filepath.IsAbs(d) {
			continue
		}
		if fspath.Within(real, fspath.Real(d)) {
			return fmt.Errorf("%s is protected", d)
		}
	}
	for _, d := range []string{env.ProjectRoot, env.DataDir} {
		if !filepath.IsAbs(d) {
			continue
		}
		if fspath.Overlap(real, fspath.Real(d)) {
			return fmt.Errorf("it overlaps %s; the project is mounted already, and mounting its parents or parts separately would bypass the read-only overlays", d)
		}
	}
	if rw {
		rel, err := filepath.Rel(home, real)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, "../") && strings.HasPrefix(rel, ".") {
			return errors.New("read-write mounts of dotfiles or anything inside dot-directories under $HOME are not allowed")
		}
		for _, d := range env.PathDirs {
			if !filepath.IsAbs(d) {
				continue
			}
			if fspath.Overlap(real, fspath.Real(d)) {
				return fmt.Errorf("read-write mount overlaps $PATH directory %s", d)
			}
		}
	}
	return nil
}

func checkDst(dst string, env Env) error {
	if dst == "/" || dst == env.Home {
		return errors.New("must not replace / or the home volume")
	}
	if env.ProjectRoot != "" && fspath.Within(dst, env.ProjectRoot) {
		return errors.New("must not shadow files of the project mount")
	}
	if env.DataDir != "" && fspath.Within(dst, env.DataDir) {
		return errors.New("must not shadow the agent-sandbox data dir (worktrees are mounted from there)")
	}
	for _, r := range reservedDsts {
		if fspath.Within(dst, r) {
			return fmt.Errorf("%s is reserved by agent-sandbox", r)
		}
	}
	return nil
}
