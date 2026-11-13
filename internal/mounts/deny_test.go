package mounts

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestDenyList(t *testing.T) {
	got := DenyList([]string{".githooks", ".git"}, []string{"secrets/", "scripts/./hooks"})
	for _, want := range []string{".git", ".envrc", ".claude/hooks", ".husky", "mise.toml", ".githooks", "secrets", "scripts/hooks"} {
		if !slices.Contains(got, want) {
			t.Errorf("DenyList lacks %q", want)
		}
	}
	if n := len(got) - len(slices.Compact(slices.Sorted(slices.Values(got)))); n != 0 {
		t.Errorf("%d duplicates in %q", n, got)
	}
}

func TestResolveDeny(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{".git", ".claude", ".vscode"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{".envrc", ".claude/settings.json", "notadir"} {
		if err := os.WriteFile(filepath.Join(root, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ResolveDeny(root, []string{".git", ".envrc", ".claude/settings.json", ".claude/hooks", ".vscode", ".idea", "notadir/x"})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{".git", ".envrc", ".claude/settings.json", ".vscode"}, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestResolveDenyRefusesSymlinks(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "id_ed25519")
	if err := os.WriteFile(secret, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, setup := range map[string]func(root string) error{
		"file symlink": func(root string) error { return os.Symlink(secret, filepath.Join(root, ".envrc")) },
		"dir symlink":  func(root string) error { return os.Symlink(outside, filepath.Join(root, ".vscode")) },
		"parent symlink": func(root string) error {
			if err := os.WriteFile(filepath.Join(outside, "settings.json"), nil, 0o644); err != nil {
				return err
			}
			return os.Symlink(outside, filepath.Join(root, ".claude"))
		},
		"dangling symlink": func(root string) error { return os.Symlink("/nonexistent", filepath.Join(root, ".mcp.json")) },
	} {
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := setup(root); err != nil {
			t.Fatal(err)
		}
		_, err = ResolveDeny(root, DenyList())
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
