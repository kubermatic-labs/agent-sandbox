// Package hostgit reads git configuration on the host without running git
// (spec 2.1): a hostile repo's settings must never execute on the host.
package hostgit

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Config holds git config entries in file order. Keys are
// "section.name" or "section.subsection.name", with section and name
// lowercased and the subsection kept as written.
type Config struct {
	entries []entry
}

type entry struct{ key, value string }

// maxIncludeDepth bounds include recursion (git uses 10 too).
const maxIncludeDepth = 10

// ParseFiles parses each file in order, following include.path and
// includeIf.*.path (unconditionally: over-approximating is the safe side
// here). Missing files are skipped.
func ParseFiles(home string, paths ...string) (Config, error) {
	var c Config
	for _, p := range paths {
		if err := c.parseFile(p, home, 0); err != nil {
			return Config{}, err
		}
	}
	return c, nil
}

// All returns every value of key, in order.
func (c Config) All(key string) []string {
	key = normalizeKey(key)
	var out []string
	for _, e := range c.entries {
		if e.key == key {
			out = append(out, e.value)
		}
	}
	return out
}

// Get returns the last value of key, as git does.
func (c Config) Get(key string) (string, bool) {
	vs := c.All(key)
	if len(vs) == 0 {
		return "", false
	}
	return vs[len(vs)-1], true
}

// normalizeKey lowercases the section and the name but not the subsection.
func normalizeKey(key string) string {
	first := strings.IndexByte(key, '.')
	last := strings.LastIndexByte(key, '.')
	if first < 0 {
		return strings.ToLower(key)
	}
	return strings.ToLower(key[:first]) + key[first:last] + strings.ToLower(key[last:])
}

func (c *Config) parseFile(path, home string, depth int) error {
	if depth > maxIncludeDepth {
		return fmt.Errorf("%s: includes nested too deeply", path)
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	section := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		// Line continuation: a trailing backslash outside quotes.
		for strings.HasSuffix(line, "\\") && !strings.HasSuffix(line, "\\\\") && sc.Scan() {
			lineNo++
			line = line[:len(line)-1] + sc.Text()
		}
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			s, rest, err := parseSection(line)
			if err != nil {
				return fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			section = s
			line = strings.TrimSpace(rest)
			if line == "" || line[0] == '#' || line[0] == ';' {
				continue
			}
		}
		if section == "" {
			return fmt.Errorf("%s:%d: key outside a section", path, lineNo)
		}
		name, value := parseKeyValue(line)
		key := section + "." + strings.ToLower(name)
		c.entries = append(c.entries, entry{key: key, value: value})

		if strings.ToLower(name) == "path" && (section == "include" || strings.HasPrefix(section, "includeif.")) {
			inc := value
			switch {
			case strings.HasPrefix(inc, "~/"):
				inc = filepath.Join(home, inc[2:])
			case !filepath.IsAbs(inc):
				inc = filepath.Join(filepath.Dir(path), inc)
			}
			if err := c.parseFile(inc, home, depth+1); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}

// parseSection parses `[section]`, `[section "sub"]` or `[section.sub]` and
// returns the normalized section prefix and whatever follows `]`.
func parseSection(line string) (string, string, error) {
	end := strings.IndexByte(line, ']')
	if end < 0 {
		return "", "", errors.New("unterminated section header")
	}
	inner, rest := strings.TrimSpace(line[1:end]), line[end+1:]
	if i := strings.IndexByte(inner, '"'); i >= 0 {
		name := strings.ToLower(strings.TrimSpace(inner[:i]))
		sub := strings.TrimSuffix(inner[i+1:], "\"")
		sub = strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(sub)
		// A quoted subsection may contain ']': re-scan for the real end.
		if j := strings.LastIndex(line, "\"]"); j > end-1 {
			sub = strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(line[strings.IndexByte(line, '"')+1 : j])
			rest = line[j+2:]
		}
		return name + "." + sub, rest, nil
	}
	return strings.ToLower(inner), rest, nil
}

// parseKeyValue splits `name = value`; a bare name is boolean true.
func parseKeyValue(line string) (string, string) {
	name, raw, ok := strings.Cut(line, "=")
	if !ok {
		return strings.TrimSpace(stripComment(line)), "true"
	}
	return strings.TrimSpace(name), unquote(raw)
}

// stripComment removes a trailing # or ; comment outside quotes.
func stripComment(s string) string {
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			inQuote = !inQuote
		case '#', ';':
			if !inQuote {
				return s[:i]
			}
		}
	}
	return s
}

// unquote handles git's value syntax: quotes, escapes, comments, and
// surrounding whitespace.
func unquote(raw string) string {
	var b strings.Builder
	inQuote := false
	pendingSpace := ""
	raw = strings.TrimLeft(raw, " \t")
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		switch {
		case ch == '\\' && i+1 < len(raw):
			i++
			b.WriteString(pendingSpace)
			pendingSpace = ""
			switch raw[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'b':
				b.WriteByte('\b')
			default:
				b.WriteByte(raw[i])
			}
		case ch == '"':
			inQuote = !inQuote
		case !inQuote && (ch == '#' || ch == ';'):
			return b.String()
		case !inQuote && (ch == ' ' || ch == '\t'):
			pendingSpace += string(ch)
		default:
			b.WriteString(pendingSpace)
			pendingSpace = ""
			b.WriteByte(ch)
		}
	}
	return b.String()
}
