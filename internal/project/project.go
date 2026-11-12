// Package project resolves the project root, its ID and derived names (spec 2.1).
//
// Root discovery is pure Go and never runs host git: a hostile repo's
// core.fsmonitor and similar settings must never run on the host.
package project

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"k8c.io/agent-sandbox/internal/fspath"
)

// Project identifies one project checkout.
type Project struct {
	Root   string // real absolute path of the project root
	HasGit bool   // a .git entry (dir or file) exists at Root
	ID     string // <sanitized basename>-<8 hex of sha256(Root)>
}

// Resolve finds the project containing cwd, which must be absolute with
// symlinks resolved.
func Resolve(cwd string) Project {
	root, hasGit := FindRoot(cwd)
	return Project{Root: root, HasGit: hasGit, ID: ComputeID(root)}
}

// FindRoot walks up from dir to the first directory containing a .git entry.
// If there is none, it returns dir and false.
func FindRoot(dir string) (string, bool) {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d, true
		}
		if filepath.Dir(d) == d {
			return dir, false
		}
	}
}

// ComputeID returns <sanitized basename>-<first 8 hex of sha256(root)>.
// The result is valid in container, volume and image repository names.
func ComputeID(root string) string {
	sum := sha256.Sum256([]byte(root))
	return sanitize(filepath.Base(root), 40, "project") + "-" + hex.EncodeToString(sum[:])[:8]
}

// sanitize lowercases s, maps every character outside [a-z0-9] to '-', trims
// dashes at both ends and truncates to max characters.
func sanitize(s string, max int, fallback string) string {
	b := []byte(strings.ToLower(s))
	for i, c := range b {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			b[i] = '-'
		}
	}
	out := strings.Trim(string(b), "-")
	if len(out) > max {
		out = strings.TrimRight(out[:max], "-")
	}
	if out == "" {
		return fallback
	}
	return out
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

// ValidateName checks a sandbox name (-n). Names end up in container, volume
// and branch names, so they are restricted to [a-z0-9-], at most 31 chars.
func ValidateName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("invalid sandbox name %q: use lowercase letters, digits and '-', starting with a letter or digit, at most 31 characters", name)
	}
	return nil
}

// ContainerName returns asb-<project-id>-<name>.
func ContainerName(id, name string) string {
	return "asb-" + id + "-" + name
}

// Hostname returns asb-<sanitized basename of root>, at most 63 characters.
func Hostname(root string) string {
	return "asb-" + sanitize(filepath.Base(root), 59, "project")
}

// MapWorkdir maps cwd, which lies under root, to the same relative location
// under mountedRoot. Anything outside root maps to mountedRoot.
func MapWorkdir(cwd, root, mountedRoot string) string {
	rel, err := filepath.Rel(root, cwd)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return mountedRoot
	}
	return filepath.Join(mountedRoot, rel)
}

// CheckRoot refuses project roots that would expose too much: home itself or
// anything above it, or anything overlapping a protected directory. home and
// root must be real paths.
func CheckRoot(root, home string, protected []string) error {
	if strings.ContainsAny(root, ":,") {
		return fmt.Errorf("project root %s contains ':' or ',', which podman mount syntax cannot express", root)
	}
	if fspath.Within(home, root) {
		return fmt.Errorf("project root %s is $HOME or above it; run agent-sandbox inside a project (a .git in $HOME, such as a dotfiles repo, also causes this)", root)
	}
	for _, d := range protected {
		if fspath.Overlap(root, fspath.Real(d)) {
			return fmt.Errorf("project root %s overlaps protected directory %s", root, d)
		}
	}
	return nil
}
