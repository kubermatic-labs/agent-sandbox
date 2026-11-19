// Package flock takes advisory locks on files.
package flock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Lock blocks until it holds an exclusive flock on path, creating the file and
// its directory (mode 0700) if needed. The returned func releases the lock.
func Lock(path string) (func(), error) {
	return lock(path, syscall.LOCK_EX)
}

// LockShared is Lock with a shared flock: any number of shared holders
// together exclude an exclusive one.
func LockShared(path string) (func(), error) {
	return lock(path, syscall.LOCK_SH)
}

// TryLock takes an exclusive flock on path without waiting. ok is false if
// another holder has it; the returned func is then nil.
func TryLock(path string) (unlock func(), ok bool, err error) {
	unlock, err = lock(path, syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, false, nil
	}
	return unlock, err == nil, err
}

func lock(path string, how int) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
