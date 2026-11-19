package trust

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// securityKeys are the config keys the trust prompt summarizes (spec 3.4).
var securityKeys = []string{"network", "allow_cidrs", "allow_domains", "git", "mounts", "readonly", "env", "worktree_copy"}

// Summary lists the security-relevant settings of the staged copy.
func (c *Check) Summary() string {
	var b strings.Builder
	cfg, err := os.ReadFile(filepath.Join(c.staging, "config.toml"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		b.WriteString("  config.toml: none\n")
	case err != nil:
		fmt.Fprintf(&b, "  config.toml: %v\n", err)
	default:
		var m map[string]any
		if _, err := toml.Decode(string(cfg), &m); err != nil {
			fmt.Fprintf(&b, "  config.toml does not parse: %v\n", err)
			break
		}
		shown := false
		for _, k := range securityKeys {
			v, ok := m[k]
			if !ok {
				continue
			}
			var buf bytes.Buffer
			if err := toml.NewEncoder(&buf).Encode(map[string]any{k: v}); err != nil {
				fmt.Fprintf(&b, "  %s = %v\n", k, v)
			} else {
				for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
					b.WriteString("  ")
					b.WriteString(line)
					b.WriteString("\n")
				}
			}
			shown = true
		}
		if !shown {
			b.WriteString("  config.toml: no security-relevant settings\n")
		}
	}
	if _, err := os.Stat(filepath.Join(c.staging, "Containerfile")); err == nil {
		b.WriteString("  Containerfile: present. It is built on the host with network access; review every RUN step.\n")
	}
	return b.String()
}

// Prompt shows the diff and summary on out and asks for approval on in.
func Prompt(c *Check, root string, in io.Reader, out io.Writer) (bool, error) {
	diff, err := c.Diff()
	if err != nil {
		return false, err
	}
	what := "has changed since you last approved it"
	if _, err := os.Stat(c.store.SnapshotDir()); errors.Is(err, fs.ErrNotExist) {
		what = "has not been approved yet"
	}
	fmt.Fprintf(out, "agent-sandbox: %s %s:\n\n%s\nSecurity-relevant settings:\n%s\n",
		filepath.Join(root, DirName), what, diff, c.Summary())
	fmt.Fprint(out, "Trust this sandbox config? [y/N] ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}
