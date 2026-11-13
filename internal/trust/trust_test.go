package trust

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func evaluate(t *testing.T, root string, store Store) *Check {
	t.Helper()
	c, err := Evaluate(root, store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Discard)
	return c
}

func TestImplicitWithoutDir(t *testing.T) {
	c := evaluate(t, t.TempDir(), NewStore(t.TempDir(), "p-1"))
	if c.State != Implicit {
		t.Errorf("State = %v, want Implicit", c.State)
	}
}

func TestLifecycle(t *testing.T) {
	root, data := t.TempDir(), t.TempDir()
	store := NewStore(data, "p-1")
	cfg := filepath.Join(root, DirName, "config.toml")
	write(t, cfg, "network = \"open\"\n", 0o644)

	c := evaluate(t, root, store)
	if c.State != Untrusted {
		t.Fatalf("first sight: %v, want Untrusted", c.State)
	}
	diff, err := c.Diff()
	if err != nil || !strings.Contains(diff, "+++ b/config.toml") || !strings.Contains(diff, "+network = \"open\"") {
		t.Errorf("first diff:\n%s %v", diff, err)
	}
	if err := c.Approve(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(store.SnapshotDir(), "config.toml")); string(b) != "network = \"open\"\n" {
		t.Errorf("snapshot = %q", b)
	}

	if c := evaluate(t, root, store); c.State != Trusted {
		t.Errorf("after approval: %v, want Trusted", c.State)
	}

	write(t, cfg, "network = \"open\"\nmounts = [{ src = \"~/.kube\", dst = \"/k\" }]\n", 0o644)
	c = evaluate(t, root, store)
	if c.State != Untrusted {
		t.Fatalf("after change: %v, want Untrusted", c.State)
	}
	diff, _ = c.Diff()
	if !strings.Contains(diff, "+mounts =") || strings.Contains(diff, "-network") {
		t.Errorf("change diff:\n%s", diff)
	}
	// Not approving keeps the old snapshot.
	c.Discard()
	if c := evaluate(t, root, store); c.State != Untrusted {
		t.Error("a discarded check must not change trust")
	}
	// A mode change alone also needs approval.
	write(t, cfg, "network = \"open\"\n", 0o600)
	if c := evaluate(t, root, store); c.State != Untrusted {
		t.Error("mode change not detected")
	}
}

func TestStagingIsCleanedUp(t *testing.T) {
	root, data := t.TempDir(), t.TempDir()
	write(t, filepath.Join(root, DirName, "config.toml"), "", 0o644)
	store := NewStore(data, "p-1")
	c, err := Evaluate(root, store)
	if err != nil {
		t.Fatal(err)
	}
	staged := c.StagedDir()
	c.Discard()
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Error("staging dir left behind")
	}
}

func TestSymlinkRefused(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, DirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(root, DirName, "passwd")); err != nil {
		t.Fatal(err)
	}
	if _, err := Evaluate(root, NewStore(t.TempDir(), "p-1")); err == nil || !strings.Contains(err.Error(), "only regular files") {
		t.Errorf("symlink accepted: %v", err)
	}
	root2 := t.TempDir()
	if err := os.Symlink("/etc", filepath.Join(root2, DirName)); err != nil {
		t.Fatal(err)
	}
	if _, err := Evaluate(root2, NewStore(t.TempDir(), "p-2")); err == nil {
		t.Error("symlinked .agent-sandbox accepted")
	}
}

func TestSummaryAndPrompt(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, DirName, "config.toml"), `
command = ["claude"]
network = "open"
allow_cidrs = ["10.0.0.1"]
env = { FOO = "bar" }
mounts = [{ src = "~/.kube/kind.yaml", dst = "~/.kube/config", ro = true }]
`, 0o644)
	write(t, filepath.Join(root, DirName, "Containerfile"), "ARG BASE_IMAGE\nFROM ${BASE_IMAGE}\n", 0o644)
	c := evaluate(t, root, NewStore(t.TempDir(), "p-1"))
	s := c.Summary()
	for _, want := range []string{`network = "open"`, `allow_cidrs = ["10.0.0.1"]`, "[env]", `FOO = "bar"`, "[[mounts]]", "Containerfile: present"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "command") {
		t.Errorf("summary lists a non-security key:\n%s", s)
	}

	var out strings.Builder
	for in, want := range map[string]bool{"y\n": true, "YES\n": true, "n\n": false, "\n": false, "": false, "sure\n": false} {
		ok, err := Prompt(c, root, strings.NewReader(in), &out)
		if err != nil || ok != want {
			t.Errorf("answer %q: %v, %v; want %v", in, ok, err, want)
		}
	}
	if !strings.Contains(out.String(), "has not been approved yet") || !strings.Contains(out.String(), "[y/N]") {
		t.Errorf("prompt output:\n%s", out.String())
	}
}

func TestSummaryBadTOML(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, DirName, "config.toml"), "network = \n", 0o644)
	c := evaluate(t, root, NewStore(t.TempDir(), "p-1"))
	if s := c.Summary(); !strings.Contains(s, "does not parse") {
		t.Errorf("summary = %q", s)
	}
}
