package project

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func tempReal(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestFindRoot(t *testing.T) {
	base := tempReal(t)

	dirRepo := filepath.Join(base, "dirrepo")
	mkdir(t, filepath.Join(dirRepo, ".git"))
	mkdir(t, filepath.Join(dirRepo, "pkg", "x"))

	fileRepo := filepath.Join(base, "filerepo")
	mkdir(t, filepath.Join(fileRepo, "sub"))
	if err := os.WriteFile(filepath.Join(fileRepo, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	nested := filepath.Join(dirRepo, "pkg", "inner")
	mkdir(t, filepath.Join(nested, ".git"))

	plain := filepath.Join(base, "plain", "deep")
	mkdir(t, plain)

	tests := []struct {
		start, root string
		hasGit      bool
	}{
		{filepath.Join(dirRepo, "pkg", "x"), dirRepo, true},
		{dirRepo, dirRepo, true},
		{filepath.Join(fileRepo, "sub"), fileRepo, true},
		{nested, nested, true},
		{plain, plain, false},
	}
	for _, tt := range tests {
		root, hasGit := FindRoot(tt.start)
		if root != tt.root || hasGit != tt.hasGit {
			t.Errorf("FindRoot(%q) = %q, %v; want %q, %v", tt.start, root, hasGit, tt.root, tt.hasGit)
		}
	}
}

func TestComputeID(t *testing.T) {
	id := ComputeID("/home/u/src/My_Project.v2")
	if !regexp.MustCompile(`^my-project-v2-[0-9a-f]{8}$`).MatchString(id) {
		t.Errorf("ComputeID = %q", id)
	}
	if id != ComputeID("/home/u/src/My_Project.v2") {
		t.Error("ComputeID must be deterministic")
	}
	if ComputeID("/a/proj") == ComputeID("/b/proj") {
		t.Error("two checkouts with the same basename must get different IDs")
	}
	if got := ComputeID("/"); !strings.HasPrefix(got, "project-") {
		t.Errorf("ComputeID(/) = %q, want project- prefix", got)
	}
	long := ComputeID("/" + strings.Repeat("a", 100))
	if len(long) != 40+1+8 {
		t.Errorf("long basename not truncated: %q", long)
	}
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"default", "a", "feature-1", strings.Repeat("a", 31)} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("ValidateName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-x", "Upper", "a_b", "a.b", "a/b", strings.Repeat("a", 32)} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("ValidateName(%q) accepted", bad)
		}
	}
}

func TestNames(t *testing.T) {
	if got := ContainerName("proj-1a2b3c4d", "default"); got != "asb-proj-1a2b3c4d-default" {
		t.Errorf("ContainerName = %q", got)
	}
	if got := Hostname("/src/My Proj"); got != "asb-my-proj" {
		t.Errorf("Hostname = %q", got)
	}
	if got := Hostname("/" + strings.Repeat("x", 100)); len(got) > 63 {
		t.Errorf("Hostname too long: %d", len(got))
	}
}

func TestMapWorkdir(t *testing.T) {
	tests := []struct{ cwd, root, mounted, want string }{
		{"/r/pkg/tasks", "/r", "/r", "/r/pkg/tasks"},
		{"/r", "/r", "/r", "/r"},
		{"/r/pkg", "/r", "/w", "/w/pkg"},
		{"/other", "/r", "/w", "/w"},
	}
	for _, tt := range tests {
		if got := MapWorkdir(tt.cwd, tt.root, tt.mounted); got != tt.want {
			t.Errorf("MapWorkdir(%q, %q, %q) = %q, want %q", tt.cwd, tt.root, tt.mounted, got, tt.want)
		}
	}
}

func TestCheckRoot(t *testing.T) {
	protected := []string{"/home/u/.config/agent-sandbox", "/home/u/.ssh"}
	ok := []string{"/home/u/src/proj", "/tmp/proj", "/home/u/.config/nvim"}
	for _, root := range ok {
		if err := CheckRoot(root, "/home/u", protected); err != nil {
			t.Errorf("CheckRoot(%q) = %v", root, err)
		}
	}
	bad := []string{
		"/home/u",                            // $HOME
		"/home",                              // above $HOME
		"/",                                  // above everything
		"/home/u/.config",                    // contains agent-sandbox config
		"/home/u/.config/agent-sandbox/base", // inside it
		"/home/u/.ssh/repo",                  // inside ~/.ssh
		"/home/u/src/a:b",                    // podman -v syntax
		"/home/u/src/a,b",                    // podman -v syntax
	}
	for _, root := range bad {
		if err := CheckRoot(root, "/home/u", protected); err == nil {
			t.Errorf("CheckRoot(%q) accepted", root)
		}
	}
}
