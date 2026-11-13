// Package trust gates the project's .agent-sandbox/ directory (spec 3.4).
//
// The directory is copied into a staging dir first, and everything after
// that (hash, diff, approval, the project image build) works on the copy,
// so what is approved is exactly what is used. Symlinks and special files
// are refused, so the build context cannot reach outside the directory.
package trust

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"k8c.io/agent-sandbox/internal/treehash"
)

// DirName is the project's sandbox config directory.
const DirName = ".agent-sandbox"

// State is the outcome of Evaluate.
type State int

const (
	// Implicit: the project has no .agent-sandbox/; only global config applies.
	Implicit State = iota
	// Trusted: the directory matches the last approved snapshot.
	Trusted
	// Untrusted: the directory is new or changed since the last approval.
	Untrusted
)

// Store is one project's trust record: <data>/trust/<project-id>/ holding
// hash and snapshot/.
type Store struct {
	dir string
}

// NewStore returns the store for a project.
func NewStore(dataDir, projectID string) Store {
	return Store{dir: filepath.Join(dataDir, "trust", projectID)}
}

// SnapshotDir is the last approved copy of .agent-sandbox/.
func (s Store) SnapshotDir() string { return filepath.Join(s.dir, "snapshot") }

func (s Store) hashFile() string { return filepath.Join(s.dir, "hash") }

// Check is the result of evaluating a project's .agent-sandbox/.
type Check struct {
	State   State
	Hash    string
	staging string
	store   Store
}

// Evaluate stages a copy of <root>/.agent-sandbox and compares its hash with
// the approved one. Call Discard when done.
func Evaluate(root string, store Store) (*Check, error) {
	src := filepath.Join(root, DirName)
	fi, err := os.Lstat(src)
	if errors.Is(err, fs.ErrNotExist) {
		return &Check{State: Implicit, store: store}, nil
	}
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s must be a directory", src)
	}
	if err := os.MkdirAll(store.dir, 0o700); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(store.dir, "staging-")
	if err != nil {
		return nil, err
	}
	c := &Check{staging: staging, store: store}
	if err := copyTree(src, staging); err != nil {
		c.Discard()
		return nil, err
	}
	if c.Hash, err = treehash.Dir(staging); err != nil {
		c.Discard()
		return nil, err
	}
	approved, err := os.ReadFile(store.hashFile())
	switch {
	case err == nil && strings.TrimSpace(string(approved)) == c.Hash:
		c.State = Trusted
		c.Discard()
	case err == nil || errors.Is(err, fs.ErrNotExist):
		c.State = Untrusted
	default:
		c.Discard()
		return nil, err
	}
	return c, nil
}

// Diff shows the staged copy against the last approved snapshot.
func (c *Check) Diff() (string, error) {
	return DiffTrees(c.store.SnapshotDir(), c.staging)
}

// StagedDir is the staged copy (only while Untrusted and not discarded).
func (c *Check) StagedDir() string { return c.staging }

// Approve makes the staged copy the approved snapshot. The hash is written
// last, so an interrupted approval asks again next time.
func (c *Check) Approve() error {
	if c.State != Untrusted || c.staging == "" {
		return errors.New("nothing to approve")
	}
	snap := c.store.SnapshotDir()
	old := snap + ".old"
	if err := os.RemoveAll(old); err != nil {
		return err
	}
	if err := os.Rename(snap, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(c.staging, snap); err != nil {
		return err
	}
	c.staging = ""
	_ = os.RemoveAll(old)
	tmp := c.store.hashFile() + ".tmp"
	if err := os.WriteFile(tmp, []byte(c.Hash+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.store.hashFile()); err != nil {
		return err
	}
	c.State = Trusted
	return nil
}

// Discard removes the staged copy, if any.
func (c *Check) Discard() {
	if c.staging != "" {
		_ = os.RemoveAll(c.staging)
		c.staging = ""
	}
}

// copyTree copies regular files and directories from src to dst (which
// exists), keeping permission bits. Anything else is refused.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if rel == "." {
				return os.Chmod(dst, info.Mode().Perm())
			}
			return os.Mkdir(target, info.Mode().Perm())
		case info.Mode().IsRegular():
			return copyFile(p, target, info.Mode().Perm())
		default:
			return fmt.Errorf("%s: only regular files and directories are allowed in %s/ (found %s)", p, DirName, info.Mode().Type())
		}
	})
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Chmod(perm); err != nil { // not subject to umask
		_ = out.Close()
		return err
	}
	return out.Close()
}
