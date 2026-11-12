// Package image builds the base and project images (spec 3.3).
package image

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"k8c.io/agent-sandbox/internal/flock"
	"k8c.io/agent-sandbox/internal/podman"
	"k8c.io/agent-sandbox/internal/treehash"
)

//go:embed base/Containerfile
var embeddedContainerfile []byte

// BaseRepo is the repository of the base image tags.
const BaseRepo = "localhost/agent-sandbox-base"

// BuildOptions are the build command flags.
type BuildOptions struct {
	NoCache bool
	Pull    bool // refresh the base image and agent versions; implies NoCache
}

// BaseContext returns the build context for the base image. If
// overrideDir/Containerfile exists, overrideDir is the context. Otherwise the
// embedded Containerfile is written to a fresh directory under workDir, which
// cleanup removes.
func BaseContext(overrideDir, workDir string) (dir string, cleanup func(), err error) {
	if _, err := os.Stat(filepath.Join(overrideDir, "Containerfile")); err == nil {
		return overrideDir, func() {}, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", nil, err
	}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return "", nil, err
	}
	dir, err = os.MkdirTemp(workDir, "base-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	if err := os.WriteFile(filepath.Join(dir, "Containerfile"), embeddedContainerfile, 0o644); err != nil {
		cleanup()
		return "", nil, err
	}
	return dir, cleanup, nil
}

// BaseTag returns the base image tag for a build context: the first 16 hex
// characters of the context's tree hash.
func BaseTag(contextDir string) (string, error) {
	h, err := treehash.Dir(contextDir)
	if err != nil {
		return "", err
	}
	return BaseRepo + ":" + h[:16], nil
}

// BuildArgs returns the podman build argv.
func BuildArgs(tag, contextDir string, opts BuildOptions) []string {
	args := []string{"build", "--tag", tag, "--file", filepath.Join(contextDir, "Containerfile")}
	switch {
	case opts.Pull:
		args = append(args, "--pull=always", "--no-cache")
	case opts.NoCache:
		args = append(args, "--no-cache")
	}
	return append(args, contextDir)
}

// EnsureBase returns the base image tag, building the image if it is missing
// or force is set. Builds run under a lock in workDir, so parallel
// invocations build once. Build output goes to out.
func EnsureBase(ctx context.Context, c podman.Client, overrideDir, workDir string, opts BuildOptions, force bool, out io.Writer) (string, error) {
	dir, cleanup, err := BaseContext(overrideDir, workDir)
	if err != nil {
		return "", fmt.Errorf("base image context: %w", err)
	}
	defer cleanup()
	tag, err := BaseTag(dir)
	if err != nil {
		return "", fmt.Errorf("base image context: %w", err)
	}

	unlock, err := flock.Lock(filepath.Join(workDir, "build-base.lock"))
	if err != nil {
		return "", err
	}
	defer unlock()

	if !force {
		ok, err := c.ImageExists(ctx, tag)
		if err != nil {
			return "", err
		}
		if ok {
			return tag, nil
		}
	}
	fmt.Fprintf(out, "agent-sandbox: building %s\n", tag)
	code, err := c.R.Stream(ctx, podman.Stdio{Out: out, Err: out}, BuildArgs(tag, dir, opts)...)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("base image build failed (podman exit %d)", code)
	}
	return tag, nil
}
