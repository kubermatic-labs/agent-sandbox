package treehash

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func hash(t *testing.T, dir string) string {
	t.Helper()
	h, err := Dir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func baseTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "Containerfile"), "FROM x\n", 0o644)
	write(t, filepath.Join(dir, "sub", "f"), "1", 0o644)
	return dir
}

func TestDirOrderIndependent(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	write(t, filepath.Join(a, "x"), "x", 0o644)
	write(t, filepath.Join(a, "y"), "y", 0o644)
	write(t, filepath.Join(b, "y"), "y", 0o644)
	write(t, filepath.Join(b, "x"), "x", 0o644)
	if hash(t, a) != hash(t, b) {
		t.Error("same tree created in different order must hash the same")
	}
}

func TestDirSensitivity(t *testing.T) {
	base := hash(t, baseTree(t))
	mutations := map[string]func(dir string){
		"content": func(dir string) { write(t, filepath.Join(dir, "sub", "f"), "2", 0o644) },
		"mode":    func(dir string) { write(t, filepath.Join(dir, "sub", "f"), "1", 0o755) },
		"rename": func(dir string) {
			if err := os.Rename(filepath.Join(dir, "sub", "f"), filepath.Join(dir, "sub", "g")); err != nil {
				t.Fatal(err)
			}
		},
		"empty dir": func(dir string) {
			if err := os.Mkdir(filepath.Join(dir, "empty"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(dir string) {
			if err := os.Symlink("/etc/passwd", filepath.Join(dir, "l")); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range mutations {
		dir := baseTree(t)
		mutate(dir)
		if hash(t, dir) == base {
			t.Errorf("%s change did not change the hash", name)
		}
	}
}

func TestDirMissing(t *testing.T) {
	if _, err := Dir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("expected error for missing dir")
	}
}
