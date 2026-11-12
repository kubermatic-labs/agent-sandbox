package flock

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLockExcludes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "x.lock")
	unlock, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}

	acquired := make(chan func())
	go func() {
		u, err := Lock(path)
		if err != nil {
			t.Error(err)
			close(acquired)
			return
		}
		acquired <- u
	}()

	select {
	case <-acquired:
		t.Fatal("second Lock succeeded while the first was held")
	case <-time.After(100 * time.Millisecond):
	}

	unlock()
	select {
	case u := <-acquired:
		if u != nil {
			u()
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Lock did not succeed after unlock")
	}
}
