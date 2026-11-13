//go:build integration

package test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeSandboxFile writes <root>/.agent-sandbox/<name>.
func (e *sandboxEnv) writeSandboxFile(name, content string) {
	e.t.Helper()
	p := filepath.Join(e.root, ".agent-sandbox", name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// trust runs `agent-sandbox trust` on a pseudo-terminal (script(1)) and
// answers the prompt.
func (e *sandboxEnv) trust(answer string) (string, int) {
	e.t.Helper()
	cmd := exec.Command("script", "-q", "-e", "-c", "'"+bin+"' trust", "/dev/null")
	cmd.Dir = e.root
	cmd.Env = e.environ()
	cmd.Stdin = strings.NewReader(answer + "\n")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	return out.String(), code
}

func (e *sandboxEnv) mustTrust() {
	e.t.Helper()
	if out, code := e.trust("y"); code != 0 {
		e.t.Fatalf("trust: exit %d\n%s", code, out)
	}
}

func (e *sandboxEnv) projectImageTags() []string {
	e.t.Helper()
	out, err := exec.Command("podman", "images", "--format", "{{.Repository}}:{{.Tag}}",
		"--filter", "reference=localhost/asb-"+e.id).Output()
	if err != nil {
		e.t.Fatal(err)
	}
	return strings.Fields(string(out))
}

const projectContainerfile = "ARG BASE_IMAGE\nFROM ${BASE_IMAGE}\nRUN echo %s > /etc/asb-project-marker\n"

func TestUntrustedConfigRefusedWithoutTTY(t *testing.T) {
	e := newProject(t, openNetwork)
	e.writeSandboxFile("config.toml", "caches = [\"go\"]\n")
	r := e.run(e.root, "", "--", "true")
	if r.code != 1 || !strings.Contains(r.stderr, "run 'agent-sandbox trust'") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestTrustLifecycle(t *testing.T) {
	e := newProject(t, openNetwork)
	e.writeSandboxFile("config.toml", "caches = [\"go\"]\n")

	out, code := e.trust("n")
	if code == 0 || !strings.Contains(out, "+caches = [\"go\"]") || !strings.Contains(out, "has not been approved yet") {
		t.Fatalf("declined trust: exit %d\n%s", code, out)
	}
	if r := e.run(e.root, "", "--", "true"); r.code != 1 {
		t.Fatal("declining must not trust")
	}

	e.mustTrust()
	e.mustRun(e.root, "", "--", "true")
	if out, code := e.trust("y"); code != 0 || !strings.Contains(out, "is trusted") {
		t.Errorf("trust on a trusted dir: exit %d\n%s", code, out)
	}

	e.writeSandboxFile("config.toml", "caches = [\"go\"]\nallow_cidrs = [\"10.0.0.1\"]\n")
	if r := e.run(e.root, "", "--", "true"); r.code != 1 || !strings.Contains(r.stderr, "untrusted") {
		t.Fatalf("a change must need approval again: exit %d, %q", r.code, r.stderr)
	}
	out, _ = e.trust("y")
	if !strings.Contains(out, "+allow_cidrs") || !strings.Contains(out, "has changed") ||
		!strings.Contains(out, `allow_cidrs = ["10.0.0.1"]`) {
		t.Errorf("change prompt:\n%s", out)
	}
	e.mustRun(e.root, "", "--", "true")
}

func TestProjectImageAndStaleWarning(t *testing.T) {
	e := newProject(t, openNetwork)
	e.writeSandboxFile("Containerfile", strings.Replace(projectContainerfile, "%s", "v1", 1))
	e.mustTrust()
	if got := e.mustRun(e.root, "", "--", "cat", "/etc/asb-project-marker"); got != "v1\n" {
		t.Fatalf("project image not used: %q", got)
	}
	before, _ := e.container()

	e.writeSandboxFile("Containerfile", strings.Replace(projectContainerfile, "%s", "v2", 1))
	e.mustTrust()
	r := e.run(e.root, "", "--", "cat", "/etc/asb-project-marker")
	if r.code != 0 || r.stdout != "v1\n" || !strings.Contains(r.stderr, "older image") {
		t.Errorf("stale sandbox: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if after, _ := e.container(); after != before {
		t.Error("a stale sandbox must keep running until stopped")
	}
	if tags := e.projectImageTags(); len(tags) != 2 {
		t.Errorf("the image the running sandbox uses must survive pruning: %q", tags)
	}

	if out, err := exec.Command("podman", "rm", "-f", "-t", "0", before).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got := e.mustRun(e.root, "", "--", "cat", "/etc/asb-project-marker"); got != "v2\n" {
		t.Errorf("after restart: %q, want v2", got)
	}
}

func TestBuildPrunesOldProjectImages(t *testing.T) {
	e := newProject(t, openNetwork)
	for _, v := range []string{"v1", "v2"} {
		e.writeSandboxFile("Containerfile", strings.Replace(projectContainerfile, "%s", v, 1))
		e.mustTrust()
		if r := e.run(e.root, "", "build"); r.code != 0 {
			t.Fatalf("build %s: %s", v, r.stderr)
		}
	}
	if tags := e.projectImageTags(); len(tags) != 1 {
		t.Errorf("old project image not pruned: %q", tags)
	}
}

func TestBuildNeedsTrust(t *testing.T) {
	e := newProject(t, openNetwork)
	e.writeSandboxFile("Containerfile", strings.Replace(projectContainerfile, "%s", "v1", 1))
	if r := e.run(e.root, "", "build"); r.code != 1 || !strings.Contains(r.stderr, "untrusted") {
		t.Errorf("build of an untrusted Containerfile: exit %d, %q", r.code, r.stderr)
	}
	if tags := e.projectImageTags(); len(tags) != 0 {
		t.Errorf("untrusted image built: %q", tags)
	}
}

func TestBadContainerfileRejected(t *testing.T) {
	e := newProject(t, openNetwork)
	e.writeSandboxFile("Containerfile", "FROM registry.fedoraproject.org/fedora:44\n")
	e.mustTrust()
	r := e.run(e.root, "", "--", "true")
	if r.code != 1 || !strings.Contains(r.stderr, "FROM ${BASE_IMAGE}") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestSymlinkInSandboxDirRefused(t *testing.T) {
	e := newProject(t, openNetwork)
	e.writeSandboxFile("config.toml", "")
	if err := os.Symlink(filepath.Join(os.Getenv("HOME"), ".ssh"), filepath.Join(e.root, ".agent-sandbox", "keys")); err != nil {
		t.Fatal(err)
	}
	r := e.run(e.root, "", "--", "true")
	if r.code != 1 || !strings.Contains(r.stderr, "only regular files") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
}
