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

func TestSharedAndTryLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	s1, err := LockShared(path)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := LockShared(path) // shared holders do not exclude each other
	if err != nil {
		t.Fatal(err)
	}
	if u, ok, err := TryLock(path); ok || err != nil || u != nil {
		t.Fatalf("TryLock while shared locks are held = %v, %v", ok, err)
	}
	s1()
	if _, ok, _ := TryLock(path); ok {
		t.Fatal("TryLock succeeded while one shared lock is still held")
	}
	s2()
	u, ok, err := TryLock(path)
	if !ok || err != nil {
		t.Fatalf("TryLock on a free lock = %v, %v", ok, err)
	}
	if _, ok, _ := TryLock(path); ok {
		t.Fatal("second TryLock succeeded")
	}
	u()
}
