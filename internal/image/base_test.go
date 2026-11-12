package image

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8c.io/agent-sandbox/internal/podman"
	"k8c.io/agent-sandbox/internal/podman/podmantest"
)

func TestBaseContextEmbedded(t *testing.T) {
	work := t.TempDir()
	dir, cleanup, err := BaseContext(filepath.Join(t.TempDir(), "no-override"), work)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "Containerfile"))
	if err != nil || !bytes.Equal(b, embeddedContainerfile) {
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

	dir2, cleanup2, err := BaseContext(filepath.Join(t.TempDir(), "no-override"), work)
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
	override := t.TempDir()
	if err := os.WriteFile(filepath.Join(override, "Containerfile"), []byte("FROM x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, cleanup, err := BaseContext(override, t.TempDir())
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
			tag, err := EnsureBase(context.Background(), podman.Client{R: f},
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
