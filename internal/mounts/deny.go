package mounts

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
)

// builtinDeny are overlaid read-only inside the mounted tree when they exist
// (spec 5.2). They are files the host reads or runs without the user
// deliberately running project code: git metadata and hooks, agent and
// editor settings, direnv/mise environments, and hook frameworks that run
// on the host commit.
var builtinDeny = []string{
	".agent-sandbox",
	".git",
	".claude/settings.json",
	".claude/settings.local.json",
	".claude/hooks",
	".mcp.json",
	".opencode",
	".codex",
	".envrc",
	".vscode",
	".idea",
	".husky",
	".pre-commit-config.yaml",
	"lefthook.yml",
	".lefthook.yml",
	"mise.toml",
	".mise.toml",
	".nvim.lua",
	".exrc",
}

// DenyList returns the built-in deny-list plus extra entries (core.hooksPath
// targets, config readonly), deduplicated, as root-relative paths.
func DenyList(extra ...[]string) []string {
	out := slices.Clone(builtinDeny)
	for _, list := range extra {
		for _, p := range list {
			p = filepath.Clean(p)
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out
}

// ResolveDeny returns the entries of deny that exist under root (a real
// path). It refuses entries reached through a symlink: a bind mount follows
// symlinks, so overlaying `.envrc -> ~/.ssh/id_ed25519` would expose the
// target inside the sandbox, and the symlink itself could not be protected.
func ResolveDeny(root string, deny []string) ([]string, error) {
	var present []string
	for _, rel := range deny {
		p := filepath.Join(root, rel)
		_, err := os.Lstat(p)
		switch {
		case err == nil:
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
			// Missing, or a parent is a file: nothing to protect.
			continue
		default:
			return nil, err
		}
		real, err := filepath.EvalSymlinks(p)
		if err != nil || real != p {
			return nil, fmt.Errorf("%s is a symlink or lies under one; agent-sandbox cannot protect it read-only. Replace the symlink with a regular file or directory", p)
		}
		present = append(present, rel)
	}
	return present, nil
}
