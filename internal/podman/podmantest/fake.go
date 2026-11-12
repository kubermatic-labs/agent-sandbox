// Package podmantest provides a scriptable fake podman.Runner.
package podmantest

import (
	"context"
	"io"
	"slices"
	"sync"

	"k8c.io/agent-sandbox/internal/podman"
)

// Result is what the fake returns for one call.
type Result struct {
	Out  string
	Code int
	Err  error
}

// Fake records every call and answers with Handler (zero Result if nil).
type Fake struct {
	Handler func(args []string) Result

	mu    sync.Mutex
	calls [][]string
}

var _ podman.Runner = (*Fake)(nil)

func (f *Fake) handle(args []string) Result {
	f.mu.Lock()
	f.calls = append(f.calls, slices.Clone(args))
	h := f.Handler
	f.mu.Unlock()
	if h == nil {
		return Result{}
	}
	return h(args)
}

// Output implements podman.Runner.
func (f *Fake) Output(_ context.Context, args ...string) ([]byte, error) {
	r := f.handle(args)
	if r.Err != nil {
		return nil, r.Err
	}
	if r.Code != 0 {
		return []byte(r.Out), &podman.ExitError{Args: args, Code: r.Code, Stderr: r.Out}
	}
	return []byte(r.Out), nil
}

// Stream implements podman.Runner; Out is written to stdio.Out.
func (f *Fake) Stream(_ context.Context, s podman.Stdio, args ...string) (int, error) {
	r := f.handle(args)
	if s.Out != nil {
		_, _ = io.WriteString(s.Out, r.Out)
	}
	return r.Code, r.Err
}

// Calls returns all recorded calls.
func (f *Fake) Calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// CallsWith returns the recorded calls whose leading args equal prefix.
func (f *Fake) CallsWith(prefix ...string) [][]string {
	var out [][]string
	for _, c := range f.Calls() {
		if len(c) >= len(prefix) && slices.Equal(c[:len(prefix)], prefix) {
			out = append(out, c)
		}
	}
	return out
}
