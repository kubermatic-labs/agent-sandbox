// Package image builds the base and project images (spec 3.3).
package image

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"k8c.io/agent-sandbox/internal/flock"
	"k8c.io/agent-sandbox/internal/labels"
	"k8c.io/agent-sandbox/internal/podman"
	"k8c.io/agent-sandbox/internal/treehash"
)

// embeddedBases holds one Containerfile per built-in base, at
// base/<distro>/Containerfile (see Distro).
//
//go:embed base/*/Containerfile
var embeddedBases embed.FS

// BaseRepo is the repository of the base image tags.
const BaseRepo = "localhost/agent-sandbox-base"

// BuildOptions are the build command flags.
type BuildOptions struct {
	NoCache bool
	Pull    bool // refresh the base image and agent versions; implies NoCache
}

// Distro returns the directory name of a base image (config key base):
// "debian" for "debian:12".
func Distro(base string) string {
	d, _, _ := strings.Cut(base, ":")
	return d
}

// EmbeddedContainerfile returns the built-in Containerfile of a base image.
func EmbeddedContainerfile(base string) ([]byte, error) {
	b, err := embeddedBases.ReadFile("base/" + Distro(base) + "/Containerfile")
	if err != nil {
		return nil, fmt.Errorf("no built-in base image %q", base)
	}
	return b, nil
}

// BaseContext returns the build context for a base image. If
// overrideRoot/<distro>/Containerfile exists, that directory is the context.
// Otherwise the embedded Containerfile is written to a fresh directory under
// workDir, which cleanup removes.
//
// overrideRoot/Containerfile, the override location from before there was a
// choice of bases, is refused rather than silently ignored.
func BaseContext(base, overrideRoot, workDir string) (dir string, cleanup func(), err error) {
	embedded, err := EmbeddedContainerfile(base)
	if err != nil {
		return "", nil, err
	}
	legacy := filepath.Join(overrideRoot, "Containerfile")
	if _, err := os.Stat(legacy); err == nil {
		return "", nil, fmt.Errorf("%s is no longer used: move the build context to %s/<distro>/ (one of fedora, debian, ubuntu, alpine)", legacy, overrideRoot)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", nil, err
	}
	overrideDir := filepath.Join(overrideRoot, Distro(base))
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
	if err := os.WriteFile(filepath.Join(dir, "Containerfile"), embedded, 0o644); err != nil {
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
	args = append(args, labels.Args(labels.BaseImage())...)
	switch {
	case opts.Pull:
		args = append(args, "--pull=always", "--no-cache")
	case opts.NoCache:
		args = append(args, "--no-cache")
	}
	return append(args, contextDir)
}

// EnsureBase returns the tag of the base image base (config key base),
// building the image if it is missing or force is set. Builds run under a lock in workDir, so parallel
// invocations build once. Build output goes to out.
func EnsureBase(ctx context.Context, c podman.Client, base, overrideRoot, workDir string, opts BuildOptions, force bool, out io.Writer) (string, error) {
	dir, cleanup, err := BaseContext(base, overrideRoot, workDir)
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
	fmt.Fprintf(out, "agent-sandbox: building %s (%s)\n", tag, base)
	code, err := c.R.Stream(ctx, podman.Stdio{Out: out, Err: out}, BuildArgs(tag, dir, opts)...)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("base image build failed (podman exit %d)", code)
	}
	return tag, nil
}
