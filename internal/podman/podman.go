// Package podman is a thin wrapper around the podman CLI. It deliberately
// avoids the podman Go bindings: every operation is a podman command line,
// so what agent-sandbox does can be reproduced by hand.
package podman

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// ExitError is a podman command that exited non-zero.
type ExitError struct {
	Args   []string
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("podman %s: exit %d: %s", strings.Join(e.Args, " "), e.Code, strings.TrimSpace(e.Stderr))
}

// Stdio connects a streamed command to the terminal or buffers.
type Stdio struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// Runner executes podman commands.
type Runner interface {
	// Output runs podman and returns its stdout. A non-zero exit is an *ExitError.
	Output(ctx context.Context, args ...string) ([]byte, error)
	// Stream runs podman attached to stdio and returns its exit code. The
	// error is non-nil only if podman could not be run at all.
	Stream(ctx context.Context, stdio Stdio, args ...string) (int, error)
}

// CLI runs the podman binary.
type CLI struct {
	Bin string // default "podman"
}

func (c CLI) bin() string {
	if c.Bin == "" {
		return "podman"
	}
	return c.Bin
}

// Output implements Runner.
func (c CLI) Output(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out, &ExitError{Args: args, Code: ee.ExitCode(), Stderr: stderr.String()}
	}
	return out, err
}

// Stream implements Runner. It is meant for long-running interactive
// commands: SIGINT and SIGQUIT reach podman directly through the terminal's
// process group, so the wrapper ignores them; SIGTERM and SIGHUP are
// forwarded. A podman killed by a signal reports 128+signal, like a shell.
func (c CLI) Stream(_ context.Context, s Stdio, args ...string) (int, error) {
	cmd := exec.Command(c.bin(), args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s.In, s.Out, s.Err

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP)
	defer func() {
		signal.Stop(sigs)
		close(sigs)
	}()

	if err := cmd.Start(); err != nil {
		return -1, err
	}
	go func() {
		for sig := range sigs {
			if sig == syscall.SIGTERM || sig == syscall.SIGHUP {
				_ = cmd.Process.Signal(sig)
			}
		}
	}()
	err := cmd.Wait()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), nil
		}
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// Client offers typed podman operations on top of a Runner.
type Client struct {
	R Runner
}

// Version returns the podman client version, e.g. "6.1.3".
func (c Client) Version(ctx context.Context) (string, error) {
	out, err := c.R.Output(ctx, "version", "--format", "{{.Client.Version}}")
	return strings.TrimSpace(string(out)), err
}

// CheckVersion refuses podman older than 5.0.
func CheckVersion(v string) error {
	major, _, _ := strings.Cut(v, ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return fmt.Errorf("cannot parse podman version %q", v)
	}
	if n < 5 {
		return fmt.Errorf("podman %s is too old; agent-sandbox needs 5.0 or newer", v)
	}
	return nil
}

// exists runs a podman "exists" command: exit 0 is true, exit 1 is false.
func (c Client) exists(ctx context.Context, args ...string) (bool, error) {
	_, err := c.R.Output(ctx, args...)
	var ee *ExitError
	if errors.As(err, &ee) && ee.Code == 1 {
		return false, nil
	}
	return err == nil, err
}

// ImageExists reports whether the image ref exists locally.
func (c Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	return c.exists(ctx, "image", "exists", ref)
}

// ImageID returns the ID of the local image ref.
func (c Client) ImageID(ctx context.Context, ref string) (string, error) {
	out, err := c.R.Output(ctx, "image", "inspect", "--format", "{{.Id}}", ref)
	return strings.TrimSpace(string(out)), err
}

// VolumeExists reports whether the named volume exists.
func (c Client) VolumeExists(ctx context.Context, name string) (bool, error) {
	return c.exists(ctx, "volume", "exists", name)
}

// VolumeCreate creates a named volume with labels.
func (c Client) VolumeCreate(ctx context.Context, name string, labels map[string]string) error {
	args := []string{"volume", "create"}
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		args = append(args, "--label", k+"="+labels[k])
	}
	_, err := c.R.Output(ctx, append(args, name)...)
	return err
}

// VolumeRemove force-removes a named volume. Force also removes every
// container that uses it: only for volumes no container can use yet.
func (c Client) VolumeRemove(ctx context.Context, name string) error {
	_, err := c.R.Output(ctx, "volume", "rm", "--force", name)
	return err
}

// VolumeRemoveUnused removes a named volume; podman refuses if a container
// uses it.
func (c Client) VolumeRemoveUnused(ctx context.Context, name string) error {
	_, err := c.R.Output(ctx, "volume", "rm", name)
	return err
}

// Container is one entry of podman ps --format json.
type Container struct {
	ID        string            `json:"Id"`
	Names     []string          `json:"Names"`
	State     string            `json:"State"`
	ExitCode  int               `json:"ExitCode"`
	Labels    map[string]string `json:"Labels"`
	StartedAt int64             `json:"StartedAt"` // unix seconds
}

// ListContainers returns all containers (running or not) carrying every label.
func (c Client) ListContainers(ctx context.Context, labels map[string]string) ([]Container, error) {
	args := []string{"ps", "--all", "--format", "json"}
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		args = append(args, "--filter", "label="+k+"="+labels[k])
	}
	out, err := c.R.Output(ctx, args...)
	if err != nil {
		return nil, err
	}
	var cs []Container
	if err := json.Unmarshal(out, &cs); err != nil {
		return nil, fmt.Errorf("parsing podman ps output: %w", err)
	}
	return cs, nil
}

// Remove force-removes a container without waiting for it to stop; a missing
// container is not an error.
func (c Client) Remove(ctx context.Context, nameOrID string) error {
	_, err := c.R.Output(ctx, "rm", "--force", "--time", "0", "--ignore", nameOrID)
	return err
}

// Stop stops a container, giving it timeout seconds before SIGKILL.
func (c Client) Stop(ctx context.Context, nameOrID string, timeout int) error {
	_, err := c.R.Output(ctx, "stop", "--ignore", "--time", strconv.Itoa(timeout), nameOrID)
	return err
}

// Process is one row of podman top.
type Process struct {
	PID  string
	User string // as the container's /etc/passwd names it
}

// Top lists a running container's processes.
func (c Client) Top(ctx context.Context, nameOrID string) ([]Process, error) {
	out, err := c.R.Output(ctx, "top", nameOrID, "pid", "user")
	if err != nil {
		return nil, err
	}
	var ps []Process
	for i, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 2 {
			continue // header
		}
		ps = append(ps, Process{PID: f[0], User: f[1]})
	}
	return ps, nil
}

// Volume is one entry of podman volume ls --format json.
type Volume struct {
	Name   string            `json:"Name"`
	Labels map[string]string `json:"Labels"`
}

// ListVolumes returns the volumes carrying every label.
func (c Client) ListVolumes(ctx context.Context, labels map[string]string) ([]Volume, error) {
	args := []string{"volume", "ls", "--format", "json"}
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		args = append(args, "--filter", "label="+k+"="+labels[k])
	}
	out, err := c.R.Output(ctx, args...)
	if err != nil {
		return nil, err
	}
	var vs []Volume
	if err := json.Unmarshal(out, &vs); err != nil {
		return nil, fmt.Errorf("parsing podman volume ls output: %w", err)
	}
	return vs, nil
}

// Image is one entry of podman images --format json.
type Image struct {
	ID         string            `json:"Id"`
	RepoTags   []string          `json:"RepoTags"`
	Labels     map[string]string `json:"Labels"`
	Containers int               `json:"Containers"` // containers using it, any state
	Dangling   bool              `json:"Dangling"`
}

// ListImages returns all local images (not intermediate build layers).
func (c Client) ListImages(ctx context.Context) ([]Image, error) {
	out, err := c.R.Output(ctx, "images", "--format", "json")
	if err != nil {
		return nil, err
	}
	var is []Image
	if err := json.Unmarshal(out, &is); err != nil {
		return nil, fmt.Errorf("parsing podman images output: %w", err)
	}
	return is, nil
}

// ImageRemove removes an image tag, or an image by ID; podman refuses if a
// container uses it.
func (c Client) ImageRemove(ctx context.Context, ref string) error {
	_, err := c.R.Output(ctx, "rmi", ref)
	return err
}

// ImageTags returns an image's tags; none means a newer build took them.
func (c Client) ImageTags(ctx context.Context, id string) ([]string, error) {
	out, err := c.R.Output(ctx, "image", "inspect", "--format", "{{range .RepoTags}}{{.}} {{end}}", id)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// MountDestinations returns the container paths of a container's mounts.
func (c Client) MountDestinations(ctx context.Context, nameOrID string) ([]string, error) {
	out, err := c.R.Output(ctx, "container", "inspect", "--format", "{{range .Mounts}}{{.Destination}}{{\"\\n\"}}{{end}}", nameOrID)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// Logs copies a container's logs to w.
func (c Client) Logs(ctx context.Context, nameOrID string, w io.Writer) error {
	code, err := c.R.Stream(ctx, Stdio{Out: w, Err: w}, "logs", nameOrID)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("podman logs %s: exit %d", nameOrID, code)
	}
	return nil
}
