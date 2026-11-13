package podman_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"k8c.io/agent-sandbox/internal/podman"
	"k8c.io/agent-sandbox/internal/podman/podmantest"
)

func fakeBin(t *testing.T, script string) podman.CLI {
	t.Helper()
	p := filepath.Join(t.TempDir(), "podman")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return podman.CLI{Bin: p}
}

func TestCLIOutput(t *testing.T) {
	out, err := fakeBin(t, `echo "out:$*"; echo noise >&2`).Output(context.Background(), "a", "b")
	if err != nil || string(out) != "out:a b\n" {
		t.Errorf("Output = %q, %v", out, err)
	}
}

func TestCLIOutputExitError(t *testing.T) {
	_, err := fakeBin(t, `echo boom >&2; exit 3`).Output(context.Background(), "x")
	var ee *podman.ExitError
	if !errors.As(err, &ee) || ee.Code != 3 || ee.Stderr != "boom\n" {
		t.Fatalf("want ExitError code 3 with stderr, got %#v", err)
	}
	if !strings.Contains(ee.Error(), "podman x: exit 3: boom") {
		t.Errorf("Error() = %q", ee.Error())
	}
}

func TestCLIStream(t *testing.T) {
	var out bytes.Buffer
	code, err := fakeBin(t, `cat; exit 7`).Stream(context.Background(),
		podman.Stdio{In: strings.NewReader("hi"), Out: &out, Err: &out})
	if err != nil || code != 7 || out.String() != "hi" {
		t.Errorf("Stream = %d, %v, %q", code, err, out.String())
	}
}

func TestCLIStreamSignal(t *testing.T) {
	code, err := fakeBin(t, `kill -TERM $$`).Stream(context.Background(), podman.Stdio{})
	if err != nil || code != 128+15 {
		t.Errorf("Stream = %d, %v; want 143", code, err)
	}
}

func TestCLIMissingBinary(t *testing.T) {
	cli := podman.CLI{Bin: filepath.Join(t.TempDir(), "nope")}
	if _, err := cli.Stream(context.Background(), podman.Stdio{}); err == nil {
		t.Error("expected error for missing binary")
	}
}

func TestCheckVersion(t *testing.T) {
	for v, ok := range map[string]bool{"5.0.0": true, "6.1.3": true, "4.9.4": false, "garbage": false, "": false} {
		if err := podman.CheckVersion(v); (err == nil) != ok {
			t.Errorf("CheckVersion(%q) = %v", v, err)
		}
	}
}

func TestClientExists(t *testing.T) {
	codes := map[string]int{"yes": 0, "no": 1, "broken": 125}
	f := &podmantest.Fake{Handler: func(args []string) podmantest.Result {
		return podmantest.Result{Code: codes[args[2]]}
	}}
	c := podman.Client{R: f}
	ctx := context.Background()
	if ok, err := c.ImageExists(ctx, "yes"); !ok || err != nil {
		t.Errorf("yes: %v, %v", ok, err)
	}
	if ok, err := c.VolumeExists(ctx, "no"); ok || err != nil {
		t.Errorf("no: %v, %v", ok, err)
	}
	if _, err := c.ImageExists(ctx, "broken"); err == nil {
		t.Error("broken: expected error")
	}
}

func TestClientCommands(t *testing.T) {
	f := &podmantest.Fake{Handler: func(args []string) podmantest.Result {
		switch args[0] {
		case "version":
			return podmantest.Result{Out: "6.1.3\n"}
		case "ps":
			return podmantest.Result{Out: `[{"Id":"abc","Names":["asb-x-default"],"State":"exited","ExitCode":2,"Labels":{"agent-sandbox":"1"}}]`}
		case "image":
			return podmantest.Result{Out: "feedface\n"}
		}
		return podmantest.Result{}
	}}
	c := podman.Client{R: f}
	ctx := context.Background()

	if v, err := c.Version(ctx); v != "6.1.3" || err != nil {
		t.Errorf("Version = %q, %v", v, err)
	}
	if id, err := c.ImageID(ctx, "img"); id != "feedface" || err != nil {
		t.Errorf("ImageID = %q, %v", id, err)
	}
	cs, err := c.ListContainers(ctx, map[string]string{"b": "2", "a": "1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []podman.Container{{ID: "abc", Names: []string{"asb-x-default"}, State: "exited", ExitCode: 2, Labels: map[string]string{"agent-sandbox": "1"}}}
	if diff := cmp.Diff(want, cs); diff != "" {
		t.Errorf("ListContainers (-want +got):\n%s", diff)
	}
	if err := c.VolumeCreate(ctx, "vol", map[string]string{"z": "1", "a": "2"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(ctx, "abc"); err != nil {
		t.Fatal(err)
	}

	wantCalls := [][]string{
		{"version", "--format", "{{.Client.Version}}"},
		{"image", "inspect", "--format", "{{.Id}}", "img"},
		{"ps", "--all", "--format", "json", "--filter", "label=a=1", "--filter", "label=b=2"},
		{"volume", "create", "--label", "a=2", "--label", "z=1", "vol"},
		{"rm", "--force", "--time", "0", "--ignore", "abc"},
	}
	if diff := cmp.Diff(wantCalls, f.Calls()); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
	}
}

func TestMountDestinations(t *testing.T) {
	f := &podmantest.Fake{Handler: func(args []string) podmantest.Result {
		return podmantest.Result{Out: "/p\n/p/.git\n/home/u\n"}
	}}
	got, err := podman.Client{R: f}.MountDestinations(context.Background(), "c")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"/p", "/p/.git", "/home/u"}, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"container", "inspect", "--format", "{{range .Mounts}}{{.Destination}}{{\"\\n\"}}{{end}}", "c"}, f.Calls()[0]); diff != "" {
		t.Errorf("argv (-want +got):\n%s", diff)
	}
}

func TestManagementCommands(t *testing.T) {
	f := &podmantest.Fake{Handler: func(args []string) podmantest.Result {
		switch args[0] {
		case "top":
			return podmantest.Result{Out: "PID         USER\n1           nobody\n7           65533\n12          kron\n"}
		case "volume":
			return podmantest.Result{Out: `[{"Name":"asb-p-1-home","Labels":{"agent-sandbox.project-id":"p-1"}}]`}
		case "image":
			return podmantest.Result{Out: "localhost/asb-p-1:abc localhost/x:y \n"}
		}
		return podmantest.Result{}
	}}
	c := podman.Client{R: f}
	ctx := context.Background()
	ps, err := c.Top(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]podman.Process{{PID: "1", User: "nobody"}, {PID: "7", User: "65533"}, {PID: "12", User: "kron"}}, ps); diff != "" {
		t.Errorf("Top (-want +got):\n%s", diff)
	}
	vs, err := c.ListVolumes(ctx, map[string]string{"agent-sandbox": "1"})
	if err != nil || len(vs) != 1 || vs[0].Labels["agent-sandbox.project-id"] != "p-1" {
		t.Errorf("ListVolumes = %+v, %v", vs, err)
	}
	tags, err := c.ImageTags(ctx, "abc")
	if err != nil || len(tags) != 2 {
		t.Errorf("ImageTags = %q, %v", tags, err)
	}
	if err := c.Stop(ctx, "c", 10); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"stop", "--ignore", "--time", "10", "c"}, f.CallsWith("stop")[0]); diff != "" {
		t.Errorf("stop argv (-want +got):\n%s", diff)
	}
}
