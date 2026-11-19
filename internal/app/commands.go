package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"k8c.io/agent-sandbox/internal/image"
	"k8c.io/agent-sandbox/internal/trust"
)

// Build force-builds the base image and, if the project has a trusted
// Containerfile, the project image.
func Build(ctx context.Context, opts image.BuildOptions) error {
	e, err := resolve()
	if err != nil {
		return err
	}
	release, err := e.holdPruneLock()
	if err != nil {
		return err
	}
	defer release()
	state, err := e.checkTrust()
	if err != nil {
		return err
	}
	cfg, err := e.loadConfig(state)
	if err != nil {
		return err
	}
	if err := e.prepare(ctx); err != nil {
		return err
	}
	_, err = e.ensureImages(ctx, state, cfg.Base, opts, true)
	return err
}

// Trust shows the project's .agent-sandbox/ against the last approved
// snapshot and asks for approval (spec 3.4).
func Trust(_ context.Context) error {
	e, err := resolve()
	if err != nil {
		return err
	}
	release, err := e.holdPruneLock()
	if err != nil {
		return err
	}
	defer release()
	dir := filepath.Join(e.proj.Root, trust.DirName)
	c, err := trust.Evaluate(e.proj.Root, e.trustStore())
	if err != nil {
		return err
	}
	defer c.Discard()
	switch c.State {
	case trust.Implicit:
		fmt.Fprintf(os.Stderr, "agent-sandbox: %s does not exist; only the global config applies\n", dir)
		return nil
	case trust.Trusted:
		fmt.Fprintf(os.Stderr, "agent-sandbox: %s is trusted\n", dir)
		return nil
	}
	if !interactive() {
		return fmt.Errorf("approving %s needs a terminal", dir)
	}
	ok, err := trust.Prompt(c, e.proj.Root, os.Stdin, os.Stderr)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s not trusted", dir)
	}
	if err := c.Approve(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "agent-sandbox: %s is trusted\n", dir)
	return nil
}
