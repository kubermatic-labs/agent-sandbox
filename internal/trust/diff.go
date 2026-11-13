package trust

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

type treeFile struct {
	mode    fs.FileMode
	content []byte
}

// readTree loads every regular file under dir. A missing dir is empty.
func readTree(dir string) (map[string]treeFile, error) {
	files := map[string]treeFile{}
	if dir == "" {
		return files, nil
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		files[filepath.ToSlash(rel)] = treeFile{mode: info.Mode().Perm(), content: b}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return files, nil
	}
	return files, err
}

// DiffTrees renders a unified diff from oldDir to newDir. oldDir may be ""
// or missing, in which case everything is new.
func DiffTrees(oldDir, newDir string) (string, error) {
	a, err := readTree(oldDir)
	if err != nil {
		return "", err
	}
	b, err := readTree(newDir)
	if err != nil {
		return "", err
	}
	var paths []string
	for p := range a {
		paths = append(paths, p)
	}
	for p := range b {
		if _, ok := a[p]; !ok {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)

	var out strings.Builder
	for _, p := range paths {
		fa, inA := a[p]
		fb, inB := b[p]
		oldName, newName := "a/"+p, "b/"+p
		switch {
		case !inA:
			oldName = "/dev/null"
			fmt.Fprintf(&out, "new file %s (mode %04o)\n", p, fb.mode)
		case !inB:
			newName = "/dev/null"
			fmt.Fprintf(&out, "deleted file %s\n", p)
		case fa.mode != fb.mode:
			fmt.Fprintf(&out, "mode change %04o -> %04o %s\n", fa.mode, fb.mode, p)
		}
		if bytes.Equal(fa.content, fb.content) {
			continue
		}
		if isBinary(fa.content) || isBinary(fb.content) {
			fmt.Fprintf(&out, "Binary file %s differs\n", p)
			continue
		}
		fmt.Fprintf(&out, "--- %s\n+++ %s\n", oldName, newName)
		out.WriteString(unified(lineDiff(splitLines(fa.content), splitLines(fb.content)), 3))
	}
	return out.String(), nil
}

func isBinary(b []byte) bool {
	return bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b)
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := strings.TrimSuffix(string(b), "\n")
	return strings.Split(s, "\n")
}

type op struct {
	kind byte // ' ', '-', '+'
	text string
}

// maxDiffCells bounds the LCS table; beyond it the whole file is shown as
// replaced.
const maxDiffCells = 4 << 20

// lineDiff returns an edit script from a to b based on the longest common
// subsequence of lines.
func lineDiff(a, b []string) []op {
	n, m := len(a), len(b)
	if n*m > maxDiffCells {
		var ops []op
		for _, l := range a {
			ops = append(ops, op{'-', l})
		}
		for _, l := range b {
			ops = append(ops, op{'+', l})
		}
		return ops
	}
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var ops []op
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, op{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, op{'-', a[i]})
			i++
		default:
			ops = append(ops, op{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, op{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, op{'+', b[j]})
	}
	return ops
}

// unified renders an edit script as unified diff hunks with ctx lines of
// context.
func unified(ops []op, ctx int) string {
	// oldBefore[i] / newBefore[i]: lines of each side before op i.
	oldBefore := make([]int, len(ops)+1)
	newBefore := make([]int, len(ops)+1)
	for i, o := range ops {
		oldBefore[i+1], newBefore[i+1] = oldBefore[i], newBefore[i]
		if o.kind != '+' {
			oldBefore[i+1]++
		}
		if o.kind != '-' {
			newBefore[i+1]++
		}
	}
	type span struct{ lo, hi int }
	var hunks []span
	for i, o := range ops {
		if o.kind == ' ' {
			continue
		}
		lo, hi := max(0, i-ctx), min(len(ops), i+ctx+1)
		if n := len(hunks); n > 0 && lo <= hunks[n-1].hi {
			hunks[n-1].hi = max(hunks[n-1].hi, hi)
			continue
		}
		hunks = append(hunks, span{lo, hi})
	}
	var out strings.Builder
	for _, h := range hunks {
		oldCount := oldBefore[h.hi] - oldBefore[h.lo]
		newCount := newBefore[h.hi] - newBefore[h.lo]
		oldStart, newStart := oldBefore[h.lo], newBefore[h.lo]
		if oldCount > 0 {
			oldStart++
		}
		if newCount > 0 {
			newStart++
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
		for _, o := range ops[h.lo:h.hi] {
			out.WriteByte(o.kind)
			out.WriteString(o.text)
			out.WriteByte('\n')
		}
	}
	return out.String()
}
