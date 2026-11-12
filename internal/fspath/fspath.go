// Package fspath has small helpers for comparing host paths.
package fspath

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
)

// Within reports whether path is dir or lies inside dir.
// Both arguments must be clean absolute paths.
func Within(path, dir string) bool {
	if path == dir {
		return true
	}
	if dir == "/" {
		return strings.HasPrefix(path, "/")
	}
	return strings.HasPrefix(path, dir+"/")
}

// Overlap reports whether a lies inside b or b lies inside a.
func Overlap(a, b string) bool {
	return Within(a, b) || Within(b, a)
}

// Real returns the absolute path p with symlinks resolved. If p does not exist,
// its longest existing ancestor is resolved and the remaining components are
// appended unchanged.
func Real(p string) string {
	p = filepath.Clean(p)
	r, err := filepath.EvalSymlinks(p)
	if err == nil {
		return r
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return p
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(Real(parent), filepath.Base(p))
}
