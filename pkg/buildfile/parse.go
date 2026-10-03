package buildfile

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// logicalLine is a (possibly continued) instruction line.
type logicalLine struct {
	text string
	line int // 1-based physical line where the instruction starts
}

// Parse reads a build description file and returns its AST. Syntax errors
// are reported as *ParseError with a 1-based line number.
func Parse(r io.Reader) (*Buildfile, error) {
	lines, err := logicalLines(r)
	if err != nil {
		return nil, err
	}
	bf := &Buildfile{byName: map[string]*Stage{}}
	var cur *Stage
	for _, ll := range lines {
		ins, err := parseInstruction(ll.text, ll.line)
		if err != nil {
			return nil, err
		}
		if ins.Kind == KindFrom {
			cur = &Stage{From: *ins, Line: ll.line, Index: len(bf.Stages)}
			name := ins.FromAs
			if name == "" {
				name = fmt.Sprintf("stage-%d", cur.Index)
			}
			cur.Name = name
			key := strings.ToLower(name)
			if _, dup := bf.byName[key]; dup {
				return nil, &ParseError{Line: ll.line, Msg: "duplicate stage name " + name}
			}
			bf.byName[key] = cur
			bf.Stages = append(bf.Stages, cur)
			continue
		}
		if cur == nil {
			return nil, &ParseError{Line: ll.line, Msg: ins.Kind.String() + " before any FROM"}
		}
		cur.Body = append(cur.Body, *ins)
	}
	if len(bf.Stages) == 0 {
		return nil, &ParseError{Msg: "no FROM instruction found"}
	}
	if err := Validate(bf); err != nil {
		return nil, err
	}
	return bf, nil
}

// logicalLines joins continuation lines and drops comments/blank lines.
func logicalLines(r io.Reader) ([]logicalLine, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	var out []logicalLine
	var buf strings.Builder
	start := 0
	n := 0
	for sc.Scan() {
		n++
		raw := sc.Text()
		trimmed := strings.TrimSpace(raw)
		if buf.Len() == 0 {
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			start = n
		}
		line := strings.TrimRight(raw, " \t")
		if strings.HasSuffix(line, "\\") {
			buf.WriteString(strings.TrimSuffix(line, "\\"))
			buf.WriteByte(' ')
			continue
		}
		buf.WriteString(line)
		out = append(out, logicalLine{text: strings.TrimSpace(buf.String()), line: start})
		buf.Reset()
	}
	if err := sc.Err(); err != nil {
		return nil, &ParseError{Msg: "read error: " + err.Error()}
	}
	if buf.Len() > 0 {
		out = append(out, logicalLine{text: strings.TrimSpace(buf.String()), line: start})
	}
	return out, nil
}

func parseInstruction(text string, line int) (*Instruction, error) {
	word, rest := splitFirst(text)
	kind, ok := parseKind(word)
	if !ok {
		return nil, &ParseError{Line: line, Msg: "unknown instruction " + word}
	}
	ins := &Instruction{Kind: kind, Line: line}
	var err error
	switch kind {
	case KindFrom:
		err = parseFrom(rest, ins)
	case KindCopy:
		err = parseCopy(rest, ins, line)
	case KindEnv, KindLabel:
		err = parsePairs(rest, ins, line)
	case KindWorkdir, KindUser:
		if rest == "" {
			err = &ParseError{Line: line, Msg: kind.String() + " requires an argument"}
		} else {
			ins.Value = rest
		}
	case KindCmd, KindEntrypoint:
		err = parseArgv(rest, ins, line)
	case KindExpose:
		if rest == "" {
			err = &ParseError{Line: line, Msg: "EXPOSE requires at least one port"}
		} else {
			ins.Ports = strings.Fields(rest)
		}
	}
	if err != nil {
		return nil, err
	}
	return ins, nil
}

func parseKind(word string) (Kind, bool) {
	switch strings.ToUpper(word) {
	case "FROM":
		return KindFrom, true
	case "COPY":
		return KindCopy, true
	case "ENV":
		return KindEnv, true
	case "LABEL":
		return KindLabel, true
	case "WORKDIR":
		return KindWorkdir, true
	case "CMD":
		return KindCmd, true
	case "ENTRYPOINT":
		return KindEntrypoint, true
	case "USER":
		return KindUser, true
	case "EXPOSE":
		return KindExpose, true
	}
	return 0, false
}

func splitFirst(s string) (string, string) {
	s = strings.TrimSpace(s)
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimSpace(s[i+1:])
}

// parseFrom parses: <ref> [AS name]
func parseFrom(rest string, ins *Instruction) error {
	f := strings.Fields(rest)
	if len(f) == 0 {
		return &ParseError{Line: ins.Line, Msg: "FROM requires a base reference"}
	}
	ins.FromRef = f[0]
	if len(f) == 1 {
		return nil
	}
	if len(f) == 3 && strings.EqualFold(f[1], "as") {
		ins.FromAs = f[2]
		return nil
	}
	return &ParseError{Line: ins.Line, Msg: "malformed FROM: expected FROM <ref> [AS name]"}
}

// parseCopy parses: [--from=stage] [--chmod=mode] src... dest
func parseCopy(rest string, ins *Instruction, line int) error {
	var args []string
	for _, tok := range strings.Fields(rest) {
		if strings.HasPrefix(tok, "--") {
			kv := strings.SplitN(strings.TrimPrefix(tok, "--"), "=", 2)
			if len(kv) != 2 {
				return &ParseError{Line: line, Msg: "COPY: flag " + tok + " needs a value"}
			}
			switch kv[0] {
			case "from":
				ins.CopyFrom = kv[1]
			case "chmod":
				ins.CopyChmod = kv[1]
			default:
				return &ParseError{Line: line, Msg: "COPY: unknown flag --" + kv[0]}
			}
			continue
		}
		args = append(args, tok)
	}
	if len(args) < 2 {
		return &ParseError{Line: line, Msg: "COPY requires at least one source and a destination"}
	}
	ins.CopySrcs = args[:len(args)-1]
	ins.CopyDest = args[len(args)-1]
	return nil
}

// parsePairs parses KEY=VALUE tokens (values may be quoted).
func parsePairs(rest string, ins *Instruction, line int) error {
	if rest == "" {
		return &ParseError{Line: line, Msg: ins.Kind.String() + " requires KEY=VALUE pairs"}
	}
	toks, err := shellFields(rest)
	if err != nil {
		return &ParseError{Line: line, Msg: err.Error()}
	}
	for _, t := range toks {
		kv := strings.SplitN(t, "=", 2)
		if len(kv) != 2 || kv[0] == "" {
			return &ParseError{Line: line, Msg: ins.Kind.String() + ": expected KEY=VALUE, got " + t}
		}
		ins.Pairs = append(ins.Pairs, KV{Key: kv[0], Value: kv[1]})
	}
	return nil
}

// parseArgv parses JSON array form or falls back to shell form.
func parseArgv(rest string, ins *Instruction, line int) error {
	if rest == "" {
		return &ParseError{Line: line, Msg: ins.Kind.String() + " requires an argument"}
	}
	if strings.HasPrefix(rest, "[") {
		var argv []string
		if err := json.Unmarshal([]byte(rest), &argv); err != nil {
			return &ParseError{Line: line, Msg: ins.Kind.String() + ": bad JSON array: " + err.Error()}
		}
		ins.Argv = argv
		return nil
	}
	ins.ShellForm = rest
	return nil
}

// shellFields splits like a minimal shell: whitespace separated, single and
// double quotes group, backslash escapes inside double quotes.
func shellFields(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inTok := false
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			if inTok {
				out = append(out, cur.String())
				cur.Reset()
				inTok = false
			}
			i++
		case c == '\'' || c == '"':
			q := c
			i++
			for i < len(s) && s[i] != q {
				if q == '"' && s[i] == '\\' && i+1 < len(s) {
					i++
				}
				cur.WriteByte(s[i])
				i++
			}
			if i >= len(s) {
				return nil, fmt.Errorf("unterminated quote")
			}
			i++
			inTok = true
		default:
			cur.WriteByte(c)
			i++
			inTok = true
		}
	}
	if inTok {
		out = append(out, cur.String())
	}
	return out, nil
}

// Validate checks cross-stage references: FROM stage names and COPY --from
// must resolve to stages defined earlier in the file.
func Validate(b *Buildfile) error {
	for _, st := range b.Stages {
		ref := st.From.FromRef
		if ref != "scratch" && !strings.HasPrefix(ref, "oci:") {
			base := b.StageByName(strings.ToLower(ref))
			if base == nil {
				return &ParseError{Line: st.Line, Msg: "FROM references unknown stage or image " + ref}
			}
			if base.Index >= st.Index {
				return &ParseError{Line: st.Line, Msg: "FROM references stage " + ref + " defined later"}
			}
		}
		for _, ins := range st.Body {
			if ins.Kind != KindCopy || ins.CopyFrom == "" {
				continue
			}
			src := b.StageByName(strings.ToLower(ins.CopyFrom))
			if src == nil {
				return &ParseError{Line: ins.Line, Msg: "COPY --from references unknown stage " + ins.CopyFrom}
			}
			if src.Index >= st.Index {
				return &ParseError{Line: ins.Line, Msg: "COPY --from references stage " + ins.CopyFrom + " defined later"}
			}
		}
	}
	return nil
}
