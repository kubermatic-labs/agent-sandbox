package scan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func mk(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if strings.HasSuffix(rel, "/") {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func realTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func paths(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Path)
	}
	return out
}

func TestScan(t *testing.T) {
	root := realTemp(t)
	mk(t, root, map[string]string{
		".git/config":                "",
		".git/modules/sub/config":    "",
		"sub/.git":                   "gitdir: ../.git/modules/sub\n", // submodule: fine
		"evil/.git":                  "gitdir: /tmp/attacker\n",       // points outside
		"a/b/.git/config":            "[core]\n\tfsmonitor = x\n",     // nested repo dir
		"node_modules/x/.git/config": "",                              // skipped dir
		"vendor/y/.git/config":       "",                              // skipped dir
		".envrc":                     "planted",
		".vscode/":                   "",
		".git-not/":                  "",
	})
	if err := os.Symlink("/etc", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "loop")); err != nil {
		t.Fatal(err)
	}

	got := Scan(Input{
		Root:       root,
		RootHadGit: true,
		GitDir:     filepath.Join(root, ".git"),
		Deny:       []string{".git", ".envrc", ".vscode", ".idea"},
		Overlaid:   map[string]bool{filepath.Join(root, ".git"): true, filepath.Join(root, ".vscode"): true},
	})
	want := []string{
		filepath.Join(root, ".envrc"),
		filepath.Join(root, "a/b/.git"),
		filepath.Join(root, "evil/.git"),
	}
	if diff := cmp.Diff(want, paths(got)); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	for _, f := range got {
		if f.Reason == "" {
			t.Errorf("%s has no reason", f.Path)
		}
	}
}

func TestScanTopLevelGitInProjectWithoutGit(t *testing.T) {
	root := realTemp(t)
	mk(t, root, map[string]string{".git/config": "[core]\n\tfsmonitor = x\n"})
	got := Scan(Input{Root: root, Deny: []string{".git"}})
	if diff := cmp.Diff([]string{filepath.Join(root, ".git")}, paths(got)); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestScanUnreadableDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads everything")
	}
	root := realTemp(t)
	mk(t, root, map[string]string{"locked/x": ""})
	if err := os.Chmod(filepath.Join(root, "locked"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "locked"), 0o755) })
	got := Scan(Input{Root: root})
	if len(got) != 1 || !strings.Contains(got[0].Reason, "could not be scanned") {
		t.Errorf("got %+v", got)
	}
}

func TestStateRoundTripAndMerge(t *testing.T) {
	dir := realTemp(t)
	state := StatePath(dir, "p-1")
	if fs, err := Load(state); err != nil || fs != nil {
		t.Fatalf("missing state: %v, %v", fs, err)
	}

	kept := filepath.Join(dir, "kept")
	gone := filepath.Join(dir, "gone")
	mk(t, dir, map[string]string{"kept": "", "new": ""})
	recorded := []Finding{{kept, "old reason"}, {gone, "deleted since"}}
	current := []Finding{{filepath.Join(dir, "new"), "new reason"}}
	merged := Merge(recorded, current)
	if diff := cmp.Diff([]string{kept, filepath.Join(dir, "new")}, paths(merged)); diff != "" {
		t.Errorf("merge (-want +got):\n%s", diff)
	}

	if err := Save(state, merged); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(state)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(merged, loaded); diff != "" {
		t.Errorf("round trip (-want +got):\n%s", diff)
	}

	if err := Save(state, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Error("empty findings must remove the state file")
	}
	if err := Save(state, nil); err != nil {
		t.Errorf("removing a missing state file: %v", err)
	}
}

func TestWarning(t *testing.T) {
	w := Warning([]Finding{{"/p/a/.git", reasonNestedGit}}, "/data/state/p/findings")
	for _, want := range []string{"WARNING", "/p/a/.git", reasonNestedGit, "/data/state/p/findings", "--strict"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning lacks %q:\n%s", want, w)
		}
	}
}
