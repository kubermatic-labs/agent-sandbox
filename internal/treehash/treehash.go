// Package treehash computes a content hash over a directory tree.
//
// The hash covers every entry's slash-separated relative path, its file mode,
// and its content (file bytes or symlink target), in lexical walk order.
// Symlinks are hashed, never followed. It is used for image tags and trust.
package treehash

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Dir returns the hex sha256 of the tree rooted at root.
func Dir(root string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		fmt.Fprintf(h, "%s\x00%o\x00", filepath.ToSlash(rel), uint32(mode))
		switch {
		case mode.IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%d\x00", len(b))
			h.Write(b)
		case mode&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%d\x00%s", len(target), target)
		case mode.IsDir():
		default:
			return fmt.Errorf("%s: unsupported file type %s", p, mode.Type())
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
