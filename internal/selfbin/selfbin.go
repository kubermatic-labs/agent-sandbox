// Package selfbin stages the running agent-sandbox binary for mounting into
// sandbox containers.
//
// The binary is copied rather than mounted in place: the container can only
// execute it after an SELinux relabel (:z), and relabeling the original
// would fail for root-owned installs such as /usr/local/bin. The copy is
// content-addressed, so a running sandbox keeps the binary it started with.
package selfbin

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"time"
)

// pruneAge is how old other versions' copies must be before Stage removes them.
const pruneAge = 24 * time.Hour

// Stage copies exe to dataDir/bin/<hash[:16]>/agent-sandbox unless that copy
// exists, and returns the copy's path and the binary's full sha256.
func Stage(exe, dataDir string) (path, hash string, err error) {
	hash, err = hashFile(exe)
	if err != nil {
		return "", "", err
	}
	binDir := filepath.Join(dataDir, "bin")
	dir := filepath.Join(binDir, hash[:16])
	path = filepath.Join(dir, "agent-sandbox")
	if fi, err := os.Lstat(path); err == nil && fi.Mode().IsRegular() {
		return path, hash, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	if err := copyAtomic(exe, path); err != nil {
		return "", "", err
	}
	prune(binDir, hash[:16])
	return path, hash, nil
}

// Hash returns the sha256 of the binary at exe, as Stage records it.
func Hash(exe string) (string, error) { return hashFile(exe) }

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyAtomic writes src to dst through a temp file and rename, so a reader
// never sees a partial binary.
func copyAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// prune removes other versions' copies older than pruneAge. A running
// sandbox is unaffected: its bind mount holds the old inode.
func prune(binDir, keep string) {
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Name() == keep {
			continue
		}
		fi, err := e.Info()
		if err != nil || time.Since(fi.ModTime()) < pruneAge {
			continue
		}
		_ = os.RemoveAll(filepath.Join(binDir, e.Name()))
	}
}
