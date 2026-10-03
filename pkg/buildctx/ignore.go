package buildctx

import (
	"fmt"
	"path"
	"strings"
)

// matcher evaluates .cibignore-style patterns against relative paths.
// Supported syntax: blank lines and # comments are skipped; a leading !
// negates; a trailing / matches directories only; * matches within one
// path segment, ** crosses segments; patterns without a slash (besides a
// trailing one) match any path segment; patterns with a slash are anchored
// at the context root.
type matcher struct {
	rules []rule
}

type rule struct {
	negate  bool
	dirOnly bool
	parts   []string // path segments of the pattern
	anchored bool
}

// compilePatterns parses ignore patterns into a matcher.
func compilePatterns(patterns []string) (*matcher, error) {
	m := &matcher{}
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		r := rule{}
		if strings.HasPrefix(p, "!") {
			r.negate = true
			p = p[1:]
		}
		p = strings.TrimPrefix(p, "/")
		if strings.HasSuffix(p, "/") {
			r.dirOnly = true
			p = strings.TrimSuffix(p, "/")
		}
		if p == "" {
			return nil, fmt.Errorf("invalid ignore pattern %q", p)
		}
		r.anchored = strings.Contains(p, "/")
		r.parts = strings.Split(p, "/")
		for _, seg := range r.parts {
			if seg == "" || seg == "." || seg == ".." {
				return nil, fmt.Errorf("invalid ignore pattern %q", p)
			}
		}
		m.rules = append(m.rules, r)
	}
	return m, nil
}

// Match reports whether path (slash-separated, relative) is ignored.
// Later rules override earlier ones; negation re-includes a path.
func (m *matcher) Match(p string, isDir bool) bool {
	ignored := false
	segs := strings.Split(p, "/")
	for _, r := range m.rules {
		if r.dirOnly && !isDir {
			continue
		}
		if matchRule(r, segs) {
			ignored = !r.negate
		}
	}
	return ignored
}

func matchRule(r rule, segs []string) bool {
	if r.anchored {
		return matchParts(r.parts, segs)
	}
	// Unanchored: pattern may start at any segment.
	for i := 0; i < len(segs); i++ {
		if matchParts(r.parts, segs[i:]) {
			return true
		}
	}
	return false
}

// matchParts matches pattern parts against path segments with ** support.
func matchParts(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if matchParts(pat[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], segs[0])
	if err != nil || !ok {
		return false
	}
	return matchParts(pat[1:], segs[1:])
}
