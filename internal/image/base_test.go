package image

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"k8c.io/agent-sandbox/internal/config"
	"k8c.io/agent-sandbox/internal/podman"
	"k8c.io/agent-sandbox/internal/podman/podmantest"
)

// Every base the config accepts has its own Containerfile, and they differ.
func TestEmbeddedContainerfiles(t *testing.T) {
	tags := map[string]string{}
	for _, base := range config.Bases {
		b, err := EmbeddedContainerfile(base)
		if err != nil {
			t.Fatal(err)
		}
		distro, version, _ := strings.Cut(base, ":")
		if !regexp.MustCompile(`(?m)^FROM \S+/` + regexp.QuoteMeta(distro+":"+version) + `$`).Match(b) {
			t.Errorf("%s: Containerfile does not build FROM %s", base, base)
		}
		for _, want := range []string{"DISABLE_AUTOUPDATER=1", "@anthropic-ai/claude-code", "process.versions.node", "ripgrep", "git"} {
			if !bytes.Contains(b, []byte(want)) {
				t.Errorf("%s: Containerfile lacks %q", base, want)
			}
		}
		dir, cleanup, err := BaseContext(base, filepath.Join(t.TempDir(), "none"), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		tag, err := BaseTag(dir)
		cleanup()
		if err != nil {
			t.Fatal(err)
		}
		if other, ok := tags[tag]; ok {
			t.Errorf("%s and %s have the same tag %s", base, other, tag)
		}
		tags[tag] = base
	}
	if _, err := EmbeddedContainerfile("gentoo:1"); err == nil {
		t.Error("unknown base accepted")
	}
}

func TestBaseContextEmbedded(t *testing.T) {
	work := t.TempDir()
	embedded, err := EmbeddedContainerfile("fedora:45")
	if err != nil {
		t.Fatal(err)
	}
	dir, cleanup, err := BaseContext("fedora:45", filepath.Join(t.TempDir(), "no-override"), work)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "Containerfile"))
	if err != nil || !bytes.Equal(b, embedded) {
		t.Fatalf("embedded Containerfile not written: %v", err)
	}
	tag1, err := BaseTag(dir)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("cleanup did not remove the context dir")
	}

	dir2, cleanup2, err := BaseContext("fedora:45", filepath.Join(t.TempDir(), "no-override"), work)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup2()
	tag2, _ := BaseTag(dir2)
	if tag1 != tag2 || !strings.HasPrefix(tag1, BaseRepo+":") || len(tag1) != len(BaseRepo)+1+16 {
		t.Errorf("tags %q and %q must be equal and well-formed", tag1, tag2)
	}
}

func TestBaseContextOverride(t *testing.T) {
	root := t.TempDir()
	override := filepath.Join(root, "debian")
	if err := os.Mkdir(override, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(override, "Containerfile"), []byte("FROM x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, cleanup, err := BaseContext("debian:12", root, t.TempDir())
	if err != nil || dir != override {
		t.Fatalf("BaseContext = %q, %v; want override dir", dir, err)
	}
	cleanup()
	if _, err := os.Stat(override); err != nil {
		t.Error("cleanup must not remove the override dir")
	}
	before, _ := BaseTag(override)
	if err := os.WriteFile(filepath.Join(override, "extra.sh"), []byte("echo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, _ := BaseTag(override)
	if before == after {
		t.Error("adding a context file must change the tag")
	}

	// The debian override does not apply to other bases.
	dir, cleanup, err = BaseContext("fedora:45", root, t.TempDir())
	if err != nil || dir == override {
		t.Fatalf("BaseContext(fedora) = %q, %v; want the embedded context", dir, err)
	}
	cleanup()
}

// The override location from before there was a choice of bases is refused,
// so an override is never silently dropped.
func TestBaseContextLegacyOverride(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Containerfile"), []byte("FROM x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := BaseContext("fedora:45", root, t.TempDir()); err == nil || !strings.Contains(err.Error(), "no longer used") {
		t.Errorf("legacy override: err = %v", err)
	}
}

func TestBuildArgs(t *testing.T) {
	for _, tt := range []struct {
		opts BuildOptions
		want []string
	}{
		{BuildOptions{}, []string{"build", "--tag", "t", "--file", "/c/Containerfile", "/c"}},
		{BuildOptions{NoCache: true}, []string{"build", "--tag", "t", "--file", "/c/Containerfile", "--no-cache", "/c"}},
		{BuildOptions{Pull: true}, []string{"build", "--tag", "t", "--file", "/c/Containerfile", "--pull=always", "--no-cache", "/c"}},
	} {
		if got := BuildArgs("t", "/c", tt.opts); !slices.Equal(got, tt.want) {
			t.Errorf("BuildArgs(%+v) = %q", tt.opts, got)
		}
	}
}

func TestEnsureBase(t *testing.T) {
	tests := []struct {
		name       string
		exists     bool
		force      bool
		buildCode  int
		wantBuilds int
		wantErr    bool
	}{
		{name: "exists", exists: true, wantBuilds: 0},
		{name: "missing", exists: false, wantBuilds: 1},
		{name: "force", exists: true, force: true, wantBuilds: 1},
		{name: "build fails", exists: false, buildCode: 1, wantBuilds: 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &podmantest.Fake{Handler: func(args []string) podmantest.Result {
				switch args[0] {
				case "image":
					if tt.exists {
						return podmantest.Result{}
					}
					return podmantest.Result{Code: 1}
				case "build":
					return podmantest.Result{Code: tt.buildCode}
				}
				return podmantest.Result{}
			}}
			var out bytes.Buffer
			tag, err := EnsureBase(context.Background(), podman.Client{R: f}, "fedora:45",
				filepath.Join(t.TempDir(), "none"), t.TempDir(), BuildOptions{}, tt.force, &out)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if !tt.wantErr && !strings.HasPrefix(tag, BaseRepo+":") {
				t.Errorf("tag = %q", tag)
			}
			if n := len(f.CallsWith("build")); n != tt.wantBuilds {
				t.Errorf("builds = %d, want %d", n, tt.wantBuilds)
			}
		})
	}
}
