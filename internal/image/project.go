package image

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"k8c.io/agent-sandbox/internal/flock"
	"k8c.io/agent-sandbox/internal/podman"
	"k8c.io/agent-sandbox/internal/treehash"
)

// ProjectRepo returns the repository of a project's image tags.
func ProjectRepo(projectID string) string {
	return "localhost/asb-" + projectID
}

// CheckProjectContainerfile requires the final stage to build on the base
// image: FROM ${BASE_IMAGE} (or $BASE_IMAGE). Earlier stages may use other
// images.
func CheckProjectContainerfile(b []byte) error {
	last := ""
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || !strings.EqualFold(f[0], "FROM") {
			continue
		}
		args := f[1:]
		for len(args) > 0 && strings.HasPrefix(args[0], "--") {
			args = args[1:]
		}
		if len(args) > 0 {
			last = args[0]
		}
	}
	if last != "${BASE_IMAGE}" && last != "$BASE_IMAGE" {
		return errors.New("the final stage of .agent-sandbox/Containerfile must be FROM ${BASE_IMAGE}, with ARG BASE_IMAGE before it")
	}
	return nil
}

// ProjectTag returns the project image tag: the first 16 hex characters of
// sha256 over the base image ID and the context's tree hash.
func ProjectTag(projectID, baseImageID, contextDir string) (string, error) {
	tree, err := treehash.Dir(contextDir)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(baseImageID + "\x00" + tree))
	return ProjectRepo(projectID) + ":" + hex.EncodeToString(sum[:])[:16], nil
}

// ProjectInput describes one project image build.
type ProjectInput struct {
	ProjectID   string
	ContextDir  string // the trusted snapshot of .agent-sandbox/
	BaseTag     string
	BaseImageID string
	WorkDir     string // holds the build lock
}

// ProjectBuildArgs returns the podman build argv. The base image is local,
// so --pull only disables the cache here; build --pull refreshes the base
// image itself.
func ProjectBuildArgs(tag string, in ProjectInput, opts BuildOptions) []string {
	args := []string{
		"build", "--tag", tag, "--file", filepath.Join(in.ContextDir, "Containerfile"),
		"--build-arg", "BASE_IMAGE=" + in.BaseTag,
	}
	if opts.NoCache || opts.Pull {
		args = append(args, "--no-cache")
	}
	return append(args, in.ContextDir)
}

// EnsureProject returns the project image tag, building it if missing or
// if force is set. After a build, older tags of the project's image that no
// container uses are removed. A failed build never touches a running
// container.
func EnsureProject(ctx context.Context, c podman.Client, in ProjectInput, opts BuildOptions, force bool, out io.Writer) (string, error) {
	cf, err := os.ReadFile(filepath.Join(in.ContextDir, "Containerfile"))
	if err != nil {
		return "", err
	}
	if err := CheckProjectContainerfile(cf); err != nil {
		return "", err
	}
	tag, err := ProjectTag(in.ProjectID, in.BaseImageID, in.ContextDir)
	if err != nil {
		return "", err
	}

	unlock, err := flock.Lock(filepath.Join(in.WorkDir, "build-"+in.ProjectID+".lock"))
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
	code, err := c.R.Stream(ctx, podman.Stdio{Out: out, Err: out}, ProjectBuildArgs(tag, in, opts)...)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("project image build failed (podman exit %d)", code)
	}
	pruneProject(ctx, c, in.ProjectID, tag)
	return tag, nil
}

// pruneProject removes the project's other image tags. Podman refuses to
// remove an image a container uses, which keeps running sandboxes intact.
func pruneProject(ctx context.Context, c podman.Client, projectID, keep string) {
	repo := ProjectRepo(projectID)
	out, err := c.R.Output(ctx, "images", "--format", "{{.Repository}}:{{.Tag}}", "--filter", "reference="+repo)
	if err != nil {
		return
	}
	for _, ref := range strings.Fields(string(out)) {
		if ref == keep || !strings.HasPrefix(ref, repo+":") {
			continue
		}
		_, _ = c.R.Output(ctx, "rmi", ref)
	}
}
