package buildctx

import (
	"bufio"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
)

// ignoreRule is a single .dockerignore pattern.
type ignoreRule struct {
	negate   bool
	dirOnly  bool
	baseName bool // pattern without a slash: matches basename at any depth
	re       *regexp.Regexp
	raw      string
}

// IgnoreMatcher evaluates .dockerignore-style patterns.
type IgnoreMatcher struct {
	rules []ignoreRule
}

// LoadIgnore reads .dockerignore from the context root (missing file = no rules).
func LoadIgnore(root string) (*IgnoreMatcher, error) {
	m := &IgnoreMatcher{}
	f, err := os.Open(path.Join(root, ".dockerignore"))
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if err := m.add(line); err != nil {
			return nil, fmt.Errorf(".dockerignore line %d: %w", lineNo, err)
		}
	}
	return m, sc.Err()
}

func (m *IgnoreMatcher) add(pat string) error {
	negate := false
	if strings.HasPrefix(pat, "!") {
		negate = true
		pat = pat[1:]
	}
	pat = strings.TrimPrefix(pat, "/")
	dirOnly := false
	if strings.HasSuffix(pat, "/") {
		dirOnly = true
		pat = strings.TrimSuffix(pat, "/")
	}
	if pat == "" {
		return fmt.Errorf("empty pattern")
	}
	baseName := !strings.Contains(pat, "/")
	re, err := globToRegex(pat)
	if err != nil {
		return err
	}
	m.rules = append(m.rules, ignoreRule{
		negate: negate, dirOnly: dirOnly, baseName: baseName, re: re, raw: pat,
	})
	return nil
}

// Match reports whether rel (slash-separated, relative to context root) is ignored.
// isDir indicates the path is a directory.
func (m *IgnoreMatcher) Match(rel string, isDir bool) bool {
	ignored := false
	for _, r := range m.rules {
		if r.dirOnly && !isDir {
			continue
		}
		matched := false
		if r.baseName {
			for _, comp := range strings.Split(rel, "/") {
				if r.re.MatchString(comp) {
					matched = true
					break
				}
			}
		} else {
			matched = r.re.MatchString(rel)
		}
		if matched {
			ignored = !r.negate
		}
	}
	return ignored
}

// globToRegex converts a dockerignore glob to an anchored regular expression.
func globToRegex(pat string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	i := 0
	for i < len(pat) {
		c := pat[i]
		switch {
		case c == '*':
			if i+1 < len(pat) && pat[i+1] == '*' {
				// ** : crosses directory separators
				i += 2
				if i < len(pat) && pat[i] == '/' {
					// **/ : zero or more leading directories
					b.WriteString("(?:.*/)?")
					i++
				} else if i == len(pat) {
					b.WriteString(".*")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
				i++
			}
		case c == '?':
			b.WriteString("[^/]")
			i++
		case c == '[':
			end := strings.IndexByte(pat[i:], ']')
			if end < 0 {
				b.WriteString("\\[")
				i++
			} else {
				b.WriteString(pat[i : i+end+1])
				i += end + 1
			}
		case c == '\\':
			if i+1 < len(pat) {
				b.WriteString(regexp.QuoteMeta(string(pat[i+1])))
				i += 2
			} else {
				b.WriteString("\\\\")
				i++
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
			i++
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
