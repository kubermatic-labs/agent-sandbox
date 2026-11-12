package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// CheckSecrets reports whether the secrets env file exists (spec 3.2). It
// refuses a file that is not a regular file or whose mode is not exactly 0600.
func CheckSecrets(path string) (bool, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !fi.Mode().IsRegular() {
		return false, fmt.Errorf("%s: not a regular file", path)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		return false, fmt.Errorf("%s: mode %04o, must be 0600; run: chmod 600 %s", path, perm, path)
	}
	return true, nil
}
