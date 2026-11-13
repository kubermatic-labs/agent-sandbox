// Package scan looks for files the host would run that a session may have
// planted (spec 7): nested .git entries (their config and hooks run when
// host git or a shell prompt touches the directory) and deny-list paths that
// are not protected read-only in the running sandbox.
package scan

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"k8c.io/agent-sandbox/internal/fspath"
	"k8c.io/agent-sandbox/internal/hostgit"
)

// Finding is one path the user should inspect.
type Finding struct {
	Path   string // absolute
	Reason string
}

// Input describes what to scan.
type Input struct {
	Root       string          // the mounted tree, a real path
	RootHadGit bool            // <Root>/.git existed when the sandbox started
	GitDir     string          // the protected top-level git dir, "" if none
	Deny       []string        // deny-list entries, root-relative
	Overlaid   map[string]bool // absolute paths overlaid read-only in the sandbox
}

const (
	reasonNestedGit   = "nested .git: git run here on the host uses its config and hooks"
	reasonUnprotected = "deny-list path not protected read-only in the running sandbox"
)

// skipDirs are not descended into (spec 7).
var skipDirs = []string{"node_modules", "vendor"}

// Scan walks the tree without following symlinks and returns the findings,
// sorted by path.
func Scan(in Input) []Finding {
	found := map[string]string{}
	topGit := filepath.Join(in.Root, ".git")
	_ = filepath.WalkDir(in.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory could hide a nested .git.
			found[p] = "could not be scanned: " + err.Error()
			if d != nil && d.IsDir() && p != in.Root {
				return filepath.SkipDir
			}
			return nil
		}
		if p == in.Root {
			return nil
		}
		name := d.Name()
		if name == ".git" {
			switch {
			case p == topGit && in.RootHadGit:
			case d.Type().IsRegular() && pointsInto(p, in.GitDir):
				// A submodule or worktree .git file into the protected git dir.
			default:
				found[p] = reasonNestedGit
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() && slices.Contains(skipDirs, name) {
			return filepath.SkipDir
		}
		return nil
	})
	for _, rel := range in.Deny {
		p := filepath.Join(in.Root, rel)
		if _, err := os.Lstat(p); err != nil || in.Overlaid[p] {
			continue
		}
		if _, ok := found[p]; !ok {
			found[p] = reasonUnprotected
		}
	}
	out := make([]Finding, 0, len(found))
	for p, r := range found {
		out = append(out, Finding{Path: p, Reason: r})
	}
	slices.SortFunc(out, func(a, b Finding) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// pointsInto reports whether the .git file at p points into gitDir.
func pointsInto(p, gitDir string) bool {
	if gitDir == "" {
		return false
	}
	target, err := hostgit.ReadGitFile(p)
	if err != nil {
		return false
	}
	return fspath.Within(fspath.Real(target), fspath.Real(gitDir))
}

// StatePath is where a project's findings are recorded.
func StatePath(dataDir, projectID string) string {
	return filepath.Join(dataDir, "state", projectID, "findings")
}

// Load reads recorded findings; a missing file has none.
func Load(path string) ([]Finding, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []Finding
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p, reason, _ := strings.Cut(sc.Text(), "\t")
		if p != "" {
			out = append(out, Finding{Path: p, Reason: reason})
		}
	}
	return out, sc.Err()
}

// StillPresent keeps the findings whose path still exists.
func StillPresent(fs []Finding) []Finding {
	var out []Finding
	for _, f := range fs {
		if _, err := os.Lstat(f.Path); err == nil {
			out = append(out, f)
		}
	}
	return out
}

// Merge combines recorded and new findings, dropping paths that no longer
// exist. Findings stay until the user removes the path or the state file:
// a planted .envrc is overlaid read-only in the next sandbox and would not
// be reported again, but the host would still run it.
func Merge(recorded, current []Finding) []Finding {
	byPath := map[string]Finding{}
	for _, f := range StillPresent(recorded) {
		byPath[f.Path] = f
	}
	for _, f := range current {
		byPath[f.Path] = f
	}
	out := make([]Finding, 0, len(byPath))
	for _, f := range byPath {
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b Finding) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// Save records findings; with none, the state file is removed.
func Save(path string, fs []Finding) error {
	if len(fs) == 0 {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	for _, f := range fs {
		fmt.Fprintf(&b, "%s\t%s\n", f.Path, f.Reason)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Warning renders findings for the terminal.
func Warning(fs []Finding, statePath string) string {
	var b strings.Builder
	b.WriteString("agent-sandbox: WARNING: the sandbox left files the host may run. Inspect or delete them before cd-ing there, committing, or opening the project in an editor:\n")
	for _, f := range fs {
		fmt.Fprintf(&b, "  %s\n      %s\n", f.Path, f.Reason)
	}
	fmt.Fprintf(&b, "Once reviewed, delete the paths or %s. With --strict, agent-sandbox refuses to start until then.\n", statePath)
	return b.String()
}
