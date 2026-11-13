package trust

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestUnified(t *testing.T) {
	a := strings.Split("1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12", "\n")
	b := strings.Split("1\n2\n3\n4\nfive\n6\n7\n8\n9\n10\n11\n12\n13", "\n")
	got := unified(lineDiff(a, b), 3)
	want := `@@ -2,7 +2,7 @@
 2
 3
 4
-5
+five
 6
 7
 8
@@ -10,3 +10,4 @@
 10
 11
 12
+13
`
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestUnifiedNewAndDeleted(t *testing.T) {
	if got := unified(lineDiff(nil, []string{"a", "b"}), 3); got != "@@ -0,0 +1,2 @@\n+a\n+b\n" {
		t.Errorf("new: %q", got)
	}
	if got := unified(lineDiff([]string{"a"}, nil), 3); got != "@@ -1,1 +0,0 @@\n-a\n" {
		t.Errorf("deleted: %q", got)
	}
	if got := unified(lineDiff([]string{"a"}, []string{"a"}), 3); got != "" {
		t.Errorf("equal: %q", got)
	}
}

func TestDiffTrees(t *testing.T) {
	old, cur := t.TempDir(), t.TempDir()
	write(t, filepath.Join(old, "config.toml"), "network = \"open\"\n", 0o644)
	write(t, filepath.Join(old, "gone.sh"), "echo\n", 0o755)
	write(t, filepath.Join(old, "same"), "x\n", 0o644)
	write(t, filepath.Join(cur, "config.toml"), "network = \"restricted\"\n", 0o644)
	write(t, filepath.Join(cur, "sub", "new.txt"), "hello\n", 0o600)
	write(t, filepath.Join(cur, "same"), "x\n", 0o755)
	write(t, filepath.Join(cur, "blob"), "\x00\x01", 0o644)

	got, err := DiffTrees(old, cur)
	if err != nil {
		t.Fatal(err)
	}
	want := `new file blob (mode 0644)
Binary file blob differs
--- a/config.toml
+++ b/config.toml
@@ -1,1 +1,1 @@
-network = "open"
+network = "restricted"
deleted file gone.sh
--- a/gone.sh
+++ /dev/null
@@ -1,1 +0,0 @@
-echo
mode change 0644 -> 0755 same
new file sub/new.txt (mode 0600)
--- /dev/null
+++ b/sub/new.txt
@@ -0,0 +1,1 @@
+hello
`
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}

	all, err := DiffTrees(filepath.Join(t.TempDir(), "missing"), cur)
	if err != nil || !strings.Contains(all, "new file config.toml") {
		t.Errorf("missing old dir: %v\n%s", err, all)
	}
}

func TestLineDiffLargeFallback(t *testing.T) {
	big := make([]string, 3000)
	for i := range big {
		big[i] = "x"
	}
	ops := lineDiff(big, big[:1500])
	if len(ops) != 4500 {
		t.Errorf("fallback ops = %d", len(ops))
	}
}
