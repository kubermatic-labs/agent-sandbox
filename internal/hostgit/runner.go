package hostgit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Runner runs host git hardened (spec 5.4): no system config, no prompts,
// no hooks, no fsmonitor, and none of the caller's GIT_* variables. It is
// used for the user's own repository, but some commands run in worktrees
// the agent controls.
type Runner struct {
	Bin string // default "git"
}

// ExitError is a git command that exited non-zero.
type ExitError struct {
	Args   []string
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("git %s: exit %d: %s", strings.Join(e.Args, " "), e.Code, strings.TrimSpace(e.Stderr))
}

// Args returns the full argv (without the binary) for running args in dir.
func Args(dir string, args ...string) []string {
	return append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}, args...)
}

// CommandEnv returns the environment for host git: env without GIT_* variables,
// plus GIT_CONFIG_NOSYSTEM=1 and GIT_TERMINAL_PROMPT=0.
func CommandEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GIT_") {
			out = append(out, kv)
		}
	}
	return append(out, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
}

// Run runs git args in dir and returns stdout. A non-zero exit is *ExitError.
func (r Runner) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	bin := r.Bin
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, Args(dir, args...)...)
	cmd.Env = CommandEnv(os.Environ())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out, &ExitError{Args: args, Code: ee.ExitCode(), Stderr: stderr.String()}
	}
	return out, err
}
