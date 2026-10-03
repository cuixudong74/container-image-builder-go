package description

import (
	"strings"
)

// Expander resolves $VAR / ${VAR} references using ARG/ENV values.
type Expander struct {
	values map[string]string
}

// NewExpander creates an expander with the given initial values.
func NewExpander(values map[string]string) *Expander {
	m := make(map[string]string, len(values))
	for k, v := range values {
		m[k] = v
	}
	return &Expander{values: m}
}

// Set sets a variable.
func (e *Expander) Set(k, v string) { e.values[k] = v }

// Get returns a variable value.
func (e *Expander) Get(k string) (string, bool) { v, ok := e.values[k]; return v, ok }

// Expand replaces variable references in s.
// Supports $VAR, ${VAR}, ${VAR:-default}, ${VAR:+word}, and $$ (literal $).
func (e *Expander) Expand(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		if c != '$' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(s) {
			b.WriteByte('$')
			i++
			continue
		}
		next := s[i+1]
		if next == '$' {
			b.WriteByte('$')
			i += 2
			continue
		}
		if next == '{' {
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				b.WriteByte('$')
				i++
				continue
			}
			expr := s[i+2 : i+2+end]
			b.WriteString(e.braceExpr(expr))
			i = i + 2 + end + 1
			continue
		}
		if isVarStart(next) {
			j := i + 1
			for j < len(s) && isVarChar(s[j]) {
				j++
			}
			name := s[i+1 : j]
			if v, ok := e.values[name]; ok {
				b.WriteString(v)
			}
			i = j
			continue
		}
		b.WriteByte('$')
		i++
	}
	return b.String()
}

func (e *Expander) braceExpr(expr string) string {
	// ${VAR:-default} or ${VAR:+word}
	for _, sep := range []string{":-", ":+"} {
		if idx := strings.Index(expr, sep); idx >= 0 {
			name := expr[:idx]
			word := expr[idx+2:]
			val, ok := e.values[name]
			if sep == ":-" {
				if !ok || val == "" {
					return e.Expand(word)
				}
				return val
			}
			// :+
			if ok && val != "" {
				return e.Expand(word)
			}
			return ""
		}
	}
	if v, ok := e.values[expr]; ok {
		return v
	}
	return ""
}

func isVarStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isVarChar(c byte) bool {
	return isVarStart(c) || (c >= '0' && c <= '9')
}
