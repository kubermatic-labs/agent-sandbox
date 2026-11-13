package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestGitIncludes(t *testing.T) {
	tree, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, ".gitconfig"), []byte("[user]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := gitIncludes([]string{".gitconfig", "conf/new.inc", ".git/local.inc"}, tree, []string{".git"})
	if err != nil {
		t.Fatal(err)
	}
	// Covered by the .git entry: neither returned nor created.
	if diff := cmp.Diff([]string{".gitconfig", "conf/new.inc"}, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	if b, err := os.ReadFile(filepath.Join(tree, ".gitconfig")); err != nil || string(b) != "[user]\n" {
		t.Errorf("existing include changed: %q, %v", b, err)
	}
	if fi, err := os.Stat(filepath.Join(tree, "conf", "new.inc")); err != nil || fi.Size() != 0 {
		t.Errorf("missing include not created empty: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tree, ".git")); err == nil {
		t.Error("created a path under a covered deny-list entry")
	}

	if err := os.Symlink(t.TempDir(), filepath.Join(tree, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := gitIncludes([]string{"linked/x.inc"}, tree, nil); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("symlinked include: err = %v", err)
	}
}
