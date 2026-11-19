package image

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"k8c.io/agent-sandbox/internal/labels"
	"k8c.io/agent-sandbox/internal/podman"
	"k8c.io/agent-sandbox/internal/podman/podmantest"
)

func TestCheckProjectContainerfile(t *testing.T) {
	ok := []string{
		"ARG BASE_IMAGE\nFROM ${BASE_IMAGE}\nRUN true\n",
		"ARG BASE_IMAGE\nfrom $BASE_IMAGE\n",
		"FROM golang:1.27 AS build\nRUN go build\nARG BASE_IMAGE\nFROM --platform=linux/amd64 ${BASE_IMAGE}\nCOPY --from=build /x /x\n",
	}
	for _, cf := range ok {
		if err := CheckProjectContainerfile([]byte(cf)); err != nil {
			t.Errorf("%q: %v", cf, err)
		}
	}
	bad := []string{
		"FROM fedora:44\n",
		"ARG BASE_IMAGE\nFROM ${BASE_IMAGE}\nFROM alpine\n",
		"RUN true\n",
		"",
	}
	for _, cf := range bad {
		if err := CheckProjectContainerfile([]byte(cf)); err == nil {
			t.Errorf("%q accepted", cf)
		}
	}
}

func projectContext(t *testing.T, containerfile string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Containerfile"), []byte(containerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestProjectTag(t *testing.T) {
	ctxDir := projectContext(t, "ARG BASE_IMAGE\nFROM ${BASE_IMAGE}\n")
	a, err := ProjectTag("p-1", "base1", ctxDir)
	if err != nil || !strings.HasPrefix(a, "localhost/asb-p-1:") || len(a) != len("localhost/asb-p-1:")+16 {
		t.Fatalf("tag = %q, %v", a, err)
	}
	if b, _ := ProjectTag("p-1", "base2", ctxDir); b == a {
		t.Error("a new base image must change the tag")
	}
	if err := os.WriteFile(filepath.Join(ctxDir, "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if c, _ := ProjectTag("p-1", "base1", ctxDir); c == a {
		t.Error("a new context file must change the tag")
	}
}

func TestProjectBuildArgs(t *testing.T) {
	in := ProjectInput{ProjectID: "p-1", ProjectRoot: "/src/p", ContextDir: "/s", BaseTag: "localhost/agent-sandbox-base:abc"}
	want := []string{
		"build", "--tag", "t", "--file", "/s/Containerfile", "--build-arg", "BASE_IMAGE=localhost/agent-sandbox-base:abc",
	}
	want = append(append(want, labels.Args(labels.ProjectImage("/src/p", "p-1"))...), "/s")
	if diff := cmp.Diff(want, ProjectBuildArgs("t", in, BuildOptions{})); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	if got := ProjectBuildArgs("t", in, BuildOptions{Pull: true}); got[len(got)-2] != "--no-cache" {
		t.Errorf("pull: %q", got)
	}
}

func TestEnsureProjectBuildsAndPrunes(t *testing.T) {
	ctxDir := projectContext(t, "ARG BASE_IMAGE\nFROM ${BASE_IMAGE}\n")
	in := ProjectInput{ProjectID: "p-1", ContextDir: ctxDir, BaseTag: "localhost/agent-sandbox-base:abc", BaseImageID: "b1", WorkDir: t.TempDir()}
	tag, _ := ProjectTag("p-1", "b1", ctxDir)
	f := &podmantest.Fake{Handler: func(args []string) podmantest.Result {
		switch args[0] {
		case "image":
			return podmantest.Result{Code: 1}
		case "images":
			return podmantest.Result{Out: tag + "\nlocalhost/asb-p-1:old1\nlocalhost/asb-p-1:old2\n"}
		case "rmi":
			if args[1] == "localhost/asb-p-1:old2" {
				return podmantest.Result{Code: 2, Out: "image is in use by a container"}
			}
		}
		return podmantest.Result{}
	}}
	got, err := EnsureProject(context.Background(), podman.Client{R: f}, in, BuildOptions{}, false, &bytes.Buffer{})
	if err != nil || got != tag {
		t.Fatalf("EnsureProject = %q, %v", got, err)
	}
	if len(f.CallsWith("build")) != 1 {
		t.Error("image not built")
	}
	if diff := cmp.Diff([][]string{{"rmi", "localhost/asb-p-1:old1"}, {"rmi", "localhost/asb-p-1:old2"}}, f.CallsWith("rmi")); diff != "" {
		t.Errorf("rmi calls (-want +got):\n%s", diff)
	}
}

func TestEnsureProjectExistingAndFailures(t *testing.T) {
	ctxDir := projectContext(t, "ARG BASE_IMAGE\nFROM ${BASE_IMAGE}\n")
	in := ProjectInput{ProjectID: "p-1", ContextDir: ctxDir, BaseImageID: "b1", WorkDir: t.TempDir()}

	f := &podmantest.Fake{}
	if _, err := EnsureProject(context.Background(), podman.Client{R: f}, in, BuildOptions{}, false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(f.CallsWith("build")) != 0 {
		t.Error("existing image rebuilt")
	}

	f = &podmantest.Fake{Handler: func(args []string) podmantest.Result {
		if args[0] == "build" {
			return podmantest.Result{Code: 1}
		}
		return podmantest.Result{}
	}}
	if _, err := EnsureProject(context.Background(), podman.Client{R: f}, in, BuildOptions{}, true, &bytes.Buffer{}); err == nil {
		t.Error("failed build reported success")
	}
	if len(f.CallsWith("rmi")) != 0 {
		t.Error("pruned after a failed build")
	}

	bad := in
	bad.ContextDir = projectContext(t, "FROM fedora\n")
	if _, err := EnsureProject(context.Background(), podman.Client{R: &podmantest.Fake{}}, bad, BuildOptions{}, false, &bytes.Buffer{}); err == nil {
		t.Error("Containerfile without FROM ${BASE_IMAGE} accepted")
	}
}
