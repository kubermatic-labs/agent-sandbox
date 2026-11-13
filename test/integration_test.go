//go:build integration

// Integration tests against real rootless podman. Run with `make integration`.
package test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"k8c.io/agent-sandbox/internal/project"
)

var bin, nftflushBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "asb-it-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	bin = filepath.Join(dir, "agent-sandbox")
	nftflushBin = filepath.Join(dir, "nftflush")
	for out, pkg := range map[string]string{bin: "./cmd/agent-sandbox", nftflushBin: "./test/tools/nftflush"} {
		build := exec.Command("go", "build", "-o", out, pkg)
		build.Dir = ".."
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		build.Stdout, build.Stderr = os.Stderr, os.Stderr
		if err := build.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "building", pkg+":", err)
			os.Exit(1)
		}
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

const openNetwork = "network = \"open\"\n"

// sandboxEnv is one throwaway git project with its own global config dir.
type sandboxEnv struct {
	t        *testing.T
	root     string
	cfgHome  string
	dataHome string
	id       string
}

func newProject(t *testing.T, globalConfig string) *sandboxEnv {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	cfgHome := t.TempDir()
	cfgDir := filepath.Join(cfgHome, "agent-sandbox")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(globalConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &sandboxEnv{t: t, root: root, cfgHome: cfgHome, dataHome: t.TempDir(), id: project.ComputeID(root)}
	t.Cleanup(e.cleanup)
	return e
}

func (e *sandboxEnv) cleanup() {
	filter := "label=agent-sandbox.project-id=" + e.id
	out, _ := exec.Command("podman", "ps", "-aq", "--filter", filter).Output()
	for _, id := range strings.Fields(string(out)) {
		_ = exec.Command("podman", "rm", "-f", "-t", "0", id).Run()
	}
	out, _ = exec.Command("podman", "volume", "ls", "-q", "--filter", filter).Output()
	for _, v := range strings.Fields(string(out)) {
		_ = exec.Command("podman", "volume", "rm", "-f", v).Run()
	}
	out, _ = exec.Command("podman", "images", "--format", "{{.Repository}}:{{.Tag}}", "--filter", "reference=localhost/asb-"+e.id).Output()
	for _, ref := range strings.Fields(string(out)) {
		_ = exec.Command("podman", "rmi", "-f", ref).Run()
	}
}

// environ is the environment agent-sandbox runs with in tests. It must not
// change XDG_CONFIG_HOME or XDG_DATA_HOME: podman reads its configuration and
// image store from those.
func (e *sandboxEnv) environ() []string {
	return append(os.Environ(),
		"AGENT_SANDBOX_CONFIG_DIR="+filepath.Join(e.cfgHome, "agent-sandbox"),
		"AGENT_SANDBOX_DATA_DIR="+e.dataHome)
}

type result struct {
	stdout, stderr string
	code           int
}

// run runs agent-sandbox in dir. It is safe to call from goroutines.
func (e *sandboxEnv) run(dir, stdin string, args ...string) result {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = e.environ()
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	r := result{stdout: out.String(), stderr: errb.String()}
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		r.code = ee.ExitCode()
	case err != nil:
		r.code, r.stderr = -1, err.Error()
	}
	return r
}

func (e *sandboxEnv) mustRun(dir, stdin string, args ...string) string {
	e.t.Helper()
	r := e.run(dir, stdin, args...)
	if r.code != 0 {
		e.t.Fatalf("agent-sandbox %q: exit %d\nstdout: %s\nstderr: %s", args, r.code, r.stdout, r.stderr)
	}
	return r.stdout
}

func TestWorkdirIsMappedFromCwd(t *testing.T) {
	e := newProject(t, openNetwork)
	sub := filepath.Join(e.root, "pkg", "tasks")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := e.mustRun(sub, "", "--", "pwd"); got != sub+"\n" {
		t.Errorf("pwd = %q, want %q", got, sub)
	}
}

func TestExitCodePassthrough(t *testing.T) {
	e := newProject(t, openNetwork)
	if r := e.run(e.root, "", "--", "sh", "-c", "exit 7"); r.code != 7 {
		t.Errorf("exit %d, want 7; stderr %s", r.code, r.stderr)
	}
}

func TestStdinIsPiped(t *testing.T) {
	e := newProject(t, openNetwork)
	if got := e.mustRun(e.root, "hello\n", "--", "cat"); got != "hello\n" {
		t.Errorf("cat = %q", got)
	}
}

func TestUnknownPositionalRejected(t *testing.T) {
	e := newProject(t, openNetwork)
	r := e.run(e.root, "", "claude")
	if r.code != 1 || !strings.Contains(r.stderr, "put the command after --") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestAllowlistNetworkFailsClosed(t *testing.T) {
	e := newProject(t, "network = \"allowlist\"\n")
	r := e.run(e.root, "", "--", "true")
	if r.code != 1 || !strings.Contains(r.stderr, "not implemented") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestProjectWritableGitReadOnly(t *testing.T) {
	e := newProject(t, openNetwork)
	e.mustRun(e.root, "", "--", "sh", "-c", "echo hi > file.txt")
	if b, err := os.ReadFile(filepath.Join(e.root, "file.txt")); err != nil || string(b) != "hi\n" {
		t.Errorf("file written in the sandbox not visible on the host: %q, %v", b, err)
	}
	for _, target := range []string{".git/hooks/x", ".git/config"} {
		if r := e.run(e.root, "", "--", "sh", "-c", "echo x >> "+target); r.code == 0 {
			t.Errorf("writing %s succeeded", target)
		}
	}
	if _, err := os.Stat(filepath.Join(e.root, ".git", "hooks", "x")); err == nil {
		t.Error(".git/hooks/x was created on the host")
	}
}

func TestSessionHasNoPrivileges(t *testing.T) {
	e := newProject(t, openNetwork)
	got := e.mustRun(e.root, "", "--", "sh", "-c", "id -u; grep CapEff /proc/self/status")
	want := fmt.Sprintf("%d\nCapEff:\t0000000000000000\n", os.Getuid())
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestHomeVolumeIsPerProject(t *testing.T) {
	a := newProject(t, openNetwork)
	b := newProject(t, openNetwork)
	a.mustRun(a.root, "", "--", "sh", "-c", "echo a > ~/marker")
	if got := a.mustRun(a.root, "", "--", "sh", "-c", "cat ~/marker"); got != "a\n" {
		t.Errorf("project A lost its home file: %q", got)
	}
	if r := b.run(b.root, "", "--", "sh", "-c", "test -e ~/marker"); r.code != 1 {
		t.Errorf("project B sees project A's home (exit %d, stderr %s)", r.code, r.stderr)
	}
}

func TestHomeSeed(t *testing.T) {
	e := newProject(t, openNetwork)
	seed := filepath.Join(e.cfgHome, "agent-sandbox", "home")
	if err := os.MkdirAll(filepath.Join(seed, ".config", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, ".config", "x", "settings"), []byte("seeded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := e.mustRun(e.root, "", "--", "sh", "-c", "cat ~/.config/x/settings && touch ~/.config/x/settings && echo ok"); got != "seeded\nok\n" {
		t.Errorf("seeded file = %q", got)
	}
}

func TestParallelInvocationsShareOneContainer(t *testing.T) {
	e := newProject(t, openNetwork)
	var wg sync.WaitGroup
	results := make([]result, 5)
	for i := range results {
		wg.Go(func() { results[i] = e.run(e.root, "", "--", "true") })
	}
	wg.Wait()
	for i, r := range results {
		if r.code != 0 {
			t.Errorf("invocation %d: exit %d: %s", i, r.code, r.stderr)
		}
	}
	out, err := exec.Command("podman", "ps", "-aq", "--filter", "label=agent-sandbox.project-id="+e.id).Output()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Fields(string(out))); n != 1 {
		t.Errorf("containers = %d, want 1", n)
	}
}

func TestNamedSandboxesAreSeparate(t *testing.T) {
	e := newProject(t, openNetwork)
	e.mustRun(e.root, "", "--", "true")
	e.mustRun(e.root, "", "-n", "other", "--", "true")
	out, err := exec.Command("podman", "ps", "-a", "--format", "{{.Names}}", "--filter", "label=agent-sandbox.project-id="+e.id).Output()
	if err != nil {
		t.Fatal(err)
	}
	names := strings.Fields(string(out))
	if len(names) != 2 {
		t.Errorf("containers = %q, want default and other", names)
	}
}

func TestCacheVolumesWritable(t *testing.T) {
	e := newProject(t, openNetwork+"caches = [\"go\"]\n")
	got := e.mustRun(e.root, "", "--", "sh", "-c", "touch $GOCACHE/x $GOMODCACHE/x && echo $GOCACHE $GOMODCACHE")
	if got != "/cache/go-build /cache/go-mod\n" {
		t.Errorf("got %q", got)
	}
}

// container returns the ID and state of the project's default sandbox.
func (e *sandboxEnv) container() (id, state string) {
	e.t.Helper()
	out, err := exec.Command("podman", "ps", "-a", "--format", "{{.ID}} {{.State}}",
		"--filter", "label=agent-sandbox.project-id="+e.id, "--filter", "label=agent-sandbox.name=default").Output()
	if err != nil {
		e.t.Fatal(err)
	}
	id, state, _ = strings.Cut(strings.TrimSpace(string(out)), " ")
	return id, state
}

func TestInitIsUnprivileged(t *testing.T) {
	e := newProject(t, openNetwork)
	got := e.mustRun(e.root, "", "--", "sh", "-c",
		`grep -E '^(Uid|Gid|Groups|CapInh|CapPrm|CapEff|CapAmb|NoNewPrivs):' /proc/1/status; tr '\0' ' ' < /proc/1/cmdline`)
	want := "Uid:\t65534\t65534\t65534\t65534\n" +
		"Gid:\t65534\t65534\t65534\t65534\n" +
		"Groups:\t \n" +
		"CapInh:\t0000000000000000\n" +
		"CapPrm:\t0000000000000000\n" +
		"CapEff:\t0000000000000000\n" +
		"CapAmb:\t0000000000000000\n" +
		"NoNewPrivs:\t1\n" +
		fmt.Sprintf("/usr/local/bin/agent-sandbox _init --network=open --idle=10m0s --uid=%d ", os.Getuid())
	if got != want {
		t.Errorf("PID 1:\n%s\nwant:\n%s", got, want)
	}
}

func TestSessionCannotKillInit(t *testing.T) {
	e := newProject(t, openNetwork)
	e.mustRun(e.root, "", "--", "true")
	before, _ := e.container()
	if r := e.run(e.root, "", "--", "sh", "-c", "kill -9 1 || kill -TERM 1 || exit 1"); r.code == 0 {
		t.Error("signalling PID 1 succeeded")
	}
	e.mustRun(e.root, "", "--", "true")
	if after, state := e.container(); after != before || state != "running" {
		t.Errorf("sandbox changed: %s -> %s (%s)", before, after, state)
	}
}

func TestIdleTimeoutStopsContainer(t *testing.T) {
	e := newProject(t, openNetwork+"idle_timeout = \"6s\"\n")
	e.mustRun(e.root, "", "--", "true")
	deadline := time.Now().Add(40 * time.Second)
	for {
		_, state := e.container()
		if state == "exited" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sandbox still %q after idle timeout", state)
		}
		time.Sleep(time.Second)
	}
	out, _ := exec.Command("podman", "ps", "-a", "--format", "{{.ExitCode}}", "--filter", "label=agent-sandbox.project-id="+e.id).Output()
	if strings.TrimSpace(string(out)) != "0" {
		t.Errorf("idle exit code = %q, want 0", out)
	}
	// The next run replaces the exited container silently.
	r := e.run(e.root, "", "--", "true")
	if r.code != 0 || r.stderr != "" {
		t.Errorf("restart after idle: exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestStopIsPrompt(t *testing.T) {
	e := newProject(t, openNetwork)
	e.mustRun(e.root, "", "--", "true")
	id, _ := e.container()
	start := time.Now()
	if out, err := exec.Command("podman", "stop", "-t", "10", id).CombinedOutput(); err != nil {
		t.Fatalf("podman stop: %v\n%s", err, out)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("stop took %v; _init must exit promptly on SIGTERM", d)
	}
}
