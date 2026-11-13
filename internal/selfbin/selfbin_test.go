package selfbin

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStage(t *testing.T) {
	src := filepath.Join(t.TempDir(), "agent-sandbox")
	if err := os.WriteFile(src, []byte("v1"), 0o700); err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()

	path, hash, err := Stage(src, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(hash) != 64 || path != filepath.Join(data, "bin", hash[:16], "agent-sandbox") {
		t.Fatalf("Stage = %q, %q", path, hash)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "v1" {
		t.Fatalf("copy = %q, %v", b, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v", fi.Mode())
	}

	again, hash2, err := Stage(src, data)
	if err != nil || again != path || hash2 != hash {
		t.Errorf("restaging the same binary: %q, %q, %v", again, hash2, err)
	}

	// An old copy of another version is pruned; a recent one is kept.
	old := filepath.Join(data, "bin", "0000000000000000")
	recent := filepath.Join(data, "bin", "1111111111111111")
	for _, d := range []string{old, recent} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("v2"), 0o700); err != nil {
		t.Fatal(err)
	}
	path2, _, err := Stage(src, data)
	if err != nil || path2 == path {
		t.Fatalf("new version: %q, %v", path2, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old copy not pruned")
	}
	if _, err := os.Stat(recent); err != nil {
		t.Error("recent copy pruned")
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("the previous version, still recent, was pruned")
	}
}
