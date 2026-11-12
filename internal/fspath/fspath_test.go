package fspath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWithin(t *testing.T) {
	tests := []struct {
		path, dir string
		want      bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b/c", "/a/b", true},
		{"/a/bc", "/a/b", false},
		{"/a", "/a/b", false},
		{"/anything", "/", true},
		{"/", "/", true},
	}
	for _, tt := range tests {
		if got := Within(tt.path, tt.dir); got != tt.want {
			t.Errorf("Within(%q, %q) = %v, want %v", tt.path, tt.dir, got, tt.want)
		}
	}
}

func TestOverlap(t *testing.T) {
	if !Overlap("/a", "/a/b") || !Overlap("/a/b", "/a") {
		t.Error("parent and child must overlap in both directions")
	}
	if Overlap("/a/b", "/a/c") {
		t.Error("siblings must not overlap")
	}
}

func TestReal(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if got := Real(link); got != target {
		t.Errorf("Real(link) = %q, want %q", got, target)
	}
	if got, want := Real(filepath.Join(link, "missing", "x")), filepath.Join(target, "missing", "x"); got != want {
		t.Errorf("Real(missing under link) = %q, want %q", got, want)
	}
}
