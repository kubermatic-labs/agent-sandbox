package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"k8c.io/agent-sandbox/internal/mounts"
	"k8c.io/agent-sandbox/internal/podman"
	"k8c.io/agent-sandbox/internal/podman/podmantest"
)

// world is a minimal stateful podman: one container, some volumes.
type world struct {
	mu        sync.Mutex
	state     string // "", "running", "exited"
	exitCode  int
	volumes   map[string]bool
	execCodes []int // successive exec results
	vanish    bool  // the first exec finds the container gone
	initFails bool
}

func (w *world) handle(args []string) podmantest.Result {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case args[0] == "ps":
		if w.state == "" {
			return podmantest.Result{Out: "[]"}
		}
		return podmantest.Result{Out: fmt.Sprintf(`[{"Id":"c1","State":%q,"ExitCode":%d}]`, w.state, w.exitCode)}
	case args[0] == "volume" && args[1] == "exists":
		if w.volumes[args[2]] {
			return podmantest.Result{}
		}
		return podmantest.Result{Code: 1}
	case args[0] == "volume" && args[1] == "create":
		w.volumes[args[len(args)-1]] = true
		return podmantest.Result{}
	case args[0] == "volume" && args[1] == "rm":
		delete(w.volumes, args[len(args)-1])
		return podmantest.Result{}
	case args[0] == "run" && args[1] == "--rm":
		if w.initFails {
			return podmantest.Result{Code: 125, Out: "init failed"}
		}
		return podmantest.Result{}
	case args[0] == "run":
		w.state, w.exitCode = "running", 0
		return podmantest.Result{Out: "c2\n"}
	case args[0] == "rm":
		w.state = ""
		return podmantest.Result{}
	case args[0] == "logs":
		return podmantest.Result{Out: "nftables: operation not supported\n"}
	case args[0] == "exec":
		if w.vanish {
			w.vanish, w.state = false, ""
			return podmantest.Result{Code: 125}
		}
		code := 0
		if len(w.execCodes) > 0 {
			code, w.execCodes = w.execCodes[0], w.execCodes[1:]
		}
		return podmantest.Result{Code: code}
	}
	return podmantest.Result{Err: fmt.Errorf("unexpected call %q", args)}
}

func newSandbox(t *testing.T, w *world) (*Sandbox, *podmantest.Fake, *bytes.Buffer) {
	t.Helper()
	if w.volumes == nil {
		w.volumes = map[string]bool{}
	}
	f := &podmantest.Fake{Handler: w.handle}
	log := &bytes.Buffer{}
	return &Sandbox{
		Podman:       podman.Client{R: f},
		Log:          log,
		Container:    "asb-x-default",
		Lookup:       Lookup("x", "default"),
		RunArgv:      []string{"run", "-d", "--name", "asb-x-default", "img", "sleep", "infinity"},
		LockDir:      t.TempDir(),
		Volumes:      []mounts.Volume{{Name: "asb-x-home", Home: true}, {Name: "asb-x-go-mod"}},
		VolumeLabels: VolumeLabels("x"),
		VolumeImage:  "img",
		UID:          1000,
		GID:          1000,
		TmpDir:       t.TempDir(),
	}, f, log
}

var execArgv = []string{"exec", "-i", "asb-x-default", "true"}

func TestExecStartsMissingContainer(t *testing.T) {
	w := &world{execCodes: []int{7}}
	s, f, _ := newSandbox(t, w)
	code, err := s.Exec(context.Background(), execArgv)
	if err != nil || code != 7 {
		t.Fatalf("Exec = %d, %v; want 7", code, err)
	}
	if n := len(f.CallsWith("run", "-d")); n != 1 {
		t.Errorf("podman run calls = %d, want 1", n)
	}
	if n := len(f.CallsWith("volume", "create")); n != 2 {
		t.Errorf("volume creates = %d, want 2", n)
	}
	if n := len(f.CallsWith("run", "--rm")); n != 2 {
		t.Errorf("volume inits = %d, want 2", n)
	}
	if !w.volumes["asb-x-home"] || !w.volumes["asb-x-go-mod"] {
		t.Errorf("volumes = %v", w.volumes)
	}
}

func TestExecReusesRunningContainer(t *testing.T) {
	w := &world{state: "running", volumes: map[string]bool{}}
	s, f, _ := newSandbox(t, w)
	if _, err := s.Exec(context.Background(), execArgv); err != nil {
		t.Fatal(err)
	}
	if n := len(f.CallsWith("run")); n != 0 {
		t.Errorf("podman run called %d times for a running container", n)
	}
}

func TestExecReplacesExitedContainerAndPrintsLogs(t *testing.T) {
	w := &world{state: "exited", exitCode: 1}
	s, f, log := newSandbox(t, w)
	if _, err := s.Exec(context.Background(), execArgv); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "exited with code 1") || !strings.Contains(log.String(), "nftables") {
		t.Errorf("logs of the failed container not printed: %q", log.String())
	}
	if len(f.CallsWith("rm", "--force", "--ignore", "c1")) != 1 || len(f.CallsWith("run", "-d")) != 1 {
		t.Errorf("exited container not replaced: %q", f.Calls())
	}
}

func TestExecCleanExitPrintsNoLogs(t *testing.T) {
	w := &world{state: "exited", exitCode: 0}
	s, f, log := newSandbox(t, w)
	if _, err := s.Exec(context.Background(), execArgv); err != nil {
		t.Fatal(err)
	}
	if len(f.CallsWith("logs")) != 0 || log.Len() != 0 {
		t.Errorf("idle-exited container must be replaced silently; log %q", log.String())
	}
}

func TestExecRetriesOnceWhenContainerVanished(t *testing.T) {
	w := &world{state: "running", vanish: true, execCodes: []int{3}}
	s, f, log := newSandbox(t, w)
	code, err := s.Exec(context.Background(), execArgv)
	if err != nil || code != 3 {
		t.Fatalf("Exec = %d, %v; want 3 from the retry", code, err)
	}
	if n := len(f.CallsWith("exec")); n != 2 {
		t.Errorf("exec calls = %d, want 2", n)
	}
	if !strings.Contains(log.String(), "starting it again") {
		t.Errorf("log = %q", log.String())
	}
}

func TestExecNoRetryWhenContainerStillRuns(t *testing.T) {
	w := &world{state: "running", execCodes: []int{125}}
	s, f, _ := newSandbox(t, w)
	code, err := s.Exec(context.Background(), execArgv)
	if err != nil || code != 125 {
		t.Fatalf("Exec = %d, %v; want 125", code, err)
	}
	if n := len(f.CallsWith("exec")); n != 1 {
		t.Errorf("exec calls = %d, want 1", n)
	}
}

func TestVolumeInitFailureRemovesVolume(t *testing.T) {
	w := &world{initFails: true}
	s, f, _ := newSandbox(t, w)
	if _, err := s.Exec(context.Background(), execArgv); err == nil {
		t.Fatal("expected error")
	}
	if w.volumes["asb-x-home"] {
		t.Error("half-initialized volume left behind")
	}
	if len(f.CallsWith("run", "-d")) != 0 {
		t.Error("container started despite volume init failure")
	}
}

func TestHomeSeedIsStagedAndCleanedUp(t *testing.T) {
	w := &world{}
	s, f, _ := newSandbox(t, w)
	s.SeedDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(s.SeedDir, ".claude.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec(context.Background(), execArgv); err != nil {
		t.Fatal(err)
	}
	inits := f.CallsWith("run", "--rm")
	if len(inits) != 2 {
		t.Fatalf("volume inits = %d", len(inits))
	}
	var seedArg string
	for _, a := range inits[0] {
		if strings.HasSuffix(a, ":/seed:ro,Z") {
			seedArg = a
		}
	}
	if seedArg == "" || !slices.Contains(inits[0], "asb-x-home:/v:U") {
		t.Fatalf("home init without seed: %q", inits[0])
	}
	staged := strings.TrimSuffix(seedArg, ":/seed:ro,Z")
	if !strings.HasPrefix(staged, s.TmpDir) {
		t.Errorf("seed staged outside TmpDir: %s", staged)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Error("staged seed copy not removed")
	}
	for _, a := range inits[1] {
		if strings.Contains(a, "/seed") {
			t.Errorf("cache volume got the home seed: %q", inits[1])
		}
	}
}
