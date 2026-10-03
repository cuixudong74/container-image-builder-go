package description

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Parser parses build description files.
type Parser struct {
	file   string
	escape byte // escape char for line continuation, default '\\'
}

// NewParser creates a parser for the given file name (used in error messages).
func NewParser(file string) *Parser {
	return &Parser{file: file, escape: '\\'}
}

// ParseFile reads and parses a description file from disk.
func ParseFile(path string) (*BuildDescription, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read build description %q: %w", path, err)
	}
	return NewParser(path).Parse(string(data))
}

// Parse parses description content.
func (p *Parser) Parse(content string) (*BuildDescription, error) {
	lines, err := p.splitLogicalLines(content)
	if err != nil {
		return nil, err
	}
	desc := &BuildDescription{SourceFile: p.file}
	var cur *Stage
	for _, ll := range lines {
		kw, rest, perr := splitKeyword(ll.text)
		if perr != nil {
			return nil, NewParseError(p.file, ll.startLine, "%s", perr.Error())
		}
		kind, ok := parseKind(kw)
		if !ok {
			return nil, NewParseError(p.file, ll.startLine, "unknown instruction %q", kw)
		}
		if kind == InstFrom {
			fa, perr := p.parseFrom(rest, ll.startLine)
			if perr != nil {
				return nil, perr
			}
			st := Stage{
				Name:      fa.AsName,
				Index:     len(desc.Stages),
				BaseImage: fa.Image,
				Platform:  fa.Platform,
			}
			st.FromScratch = strings.EqualFold(fa.Image, "scratch")
			desc.Stages = append(desc.Stages, st)
			cur = &desc.Stages[len(desc.Stages)-1]
			continue
		}
		if cur == nil {
			if kind == InstArg {
				kv, perr := parseKVArgs(kind, rest)
				if perr != nil {
					return nil, NewParseError(p.file, ll.startLine, "%s", perr.Error())
				}
				desc.GlobalARGs = append(desc.GlobalARGs, *kv)
				continue
			}
			return nil, NewParseError(p.file, ll.startLine, "%s instruction must follow FROM", kw)
		}
		instr := &Instruction{Kind: kind, Line: ll.startLine, Raw: strings.TrimSpace(ll.text),
			Args: map[string]interface{}{}}
		if err := p.parseArgs(instr, rest); err != nil {
			return nil, NewParseError(p.file, ll.startLine, "%s", err.Error())
		}
		cur.Instructions = append(cur.Instructions, instr)
	}
	if len(desc.Stages) == 0 {
		return nil, NewParseError(p.file, 1, "build description contains no FROM stage")
	}
	return desc, nil
}

type logicalLine struct {
	text      string
	startLine int
}

// splitLogicalLines handles comments, parser directives and line continuations.
func (p *Parser) splitLogicalLines(content string) ([]logicalLine, error) {
	rawLines := strings.Split(content, "\n")
	var out []logicalLine
	var buf strings.Builder
	startLine := 0
	escape := p.escape
	for i := 0; i < len(rawLines); i++ {
		line := strings.TrimRight(rawLines[i], "\r")
		trimmed := strings.TrimSpace(line)
		if buf.Len() == 0 {
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				// Parser directive: # escape=` (backtick)
				if strings.HasPrefix(trimmed, "#") && i == 0 {
					body := strings.TrimSpace(strings.TrimPrefix(trimmed, "#"))
					if strings.HasPrefix(body, "escape=") {
						v := strings.TrimSpace(strings.TrimPrefix(body, "escape="))
						if len(v) == 1 {
							escape = v[0]
							p.escape = escape
						}
					}
				}
				continue
			}
			startLine = i + 1
			buf.WriteString(line)
		} else {
			buf.WriteString(" ")
			buf.WriteString(strings.TrimSpace(line))
		}
		// Continuation?
		if strings.HasSuffix(strings.TrimRight(line, " \t"), string(escape)) {
			// strip the trailing escape char and keep joining
			s := buf.String()
			s = strings.TrimRight(s, " \t")
			s = strings.TrimSuffix(s, string(escape))
			buf.Reset()
			buf.WriteString(s)
			continue
		}
		out = append(out, logicalLine{text: buf.String(), startLine: startLine})
		buf.Reset()
	}
	if buf.Len() > 0 {
		out = append(out, logicalLine{text: buf.String(), startLine: startLine})
	}
	return out, nil
}

func splitKeyword(text string) (string, string, error) {
	t := strings.TrimSpace(text)
	if t == "" {
		return "", "", fmt.Errorf("empty instruction")
	}
	idx := strings.IndexAny(t, " \t")
	if idx < 0 {
		return strings.ToUpper(t), "", nil
	}
	return strings.ToUpper(strings.TrimSpace(t[:idx])), strings.TrimSpace(t[idx:]), nil
}

func parseKind(kw string) (InstructionKind, bool) {
	switch strings.ToUpper(kw) {
	case "FROM":
		return InstFrom, true
	case "RUN":
		return InstRun, true
	case "CMD":
		return InstCmd, true
	case "ENTRYPOINT":
		return InstEntrypoint, true
	case "COPY":
		return InstCopy, true
	case "ADD":
		return InstAdd, true
	case "ENV":
		return InstEnv, true
	case "LABEL":
		return InstLabel, true
	case "WORKDIR":
		return InstWorkdir, true
	case "USER":
		return InstUser, true
	case "ARG":
		return InstArg, true
	case "EXPOSE":
		return InstExpose, true
	case "VOLUME":
		return InstVolume, true
	case "MAINTAINER":
		return InstMaintainer, true
	}
	return 0, false
}

func (p *Parser) parseFrom(rest string, line int) (*FromArgs, error) {
	fields := splitFields(rest)
	fa := &FromArgs{}
	i := 0
	for i < len(fields) {
		f := fields[i]
		if strings.HasPrefix(f, "--") {
			name, val, err := splitFlag(f)
			if err != nil {
				return nil, NewParseError(p.file, line, "FROM: %s", err.Error())
			}
			switch name {
			case "platform":
				fa.Platform = val
			default:
				return nil, NewParseError(p.file, line, "FROM: unknown flag --%s", name)
			}
			i++
			continue
		}
		break
	}
	if i >= len(fields) {
		return nil, NewParseError(p.file, line, "FROM: missing image reference")
	}
	fa.Image = fields[i]
	i++
	if i < len(fields) {
		if !strings.EqualFold(fields[i], "AS") {
			return nil, NewParseError(p.file, line, "FROM: expected AS, got %q", fields[i])
		}
		i++
		if i >= len(fields) {
			return nil, NewParseError(p.file, line, "FROM: AS requires a stage name")
		}
		fa.AsName = fields[i]
		i++
	}
	if i != len(fields) {
		return nil, NewParseError(p.file, line, "FROM: unexpected arguments %q", strings.Join(fields[i:], " "))
	}
	return fa, nil
}

func (p *Parser) parseArgs(instr *Instruction, rest string) error {
	switch instr.Kind {
	case InstFrom:
		// handled separately
		return nil
	case InstCopy, InstAdd:
		ca, err := parseCopyArgs(rest)
		if err != nil {
			return err
		}
		instr.Args["copy"] = ca
	case InstRun, InstCmd, InstEntrypoint:
		ra, err := parseRunArgs(rest)
		if err != nil {
			return err
		}
		instr.Args["run"] = ra
	case InstEnv, InstLabel, InstArg:
		kv, err := parseKVArgs(instr.Kind, rest)
		if err != nil {
			return err
		}
		instr.Args["kv"] = kv
	case InstWorkdir, InstUser, InstMaintainer:
		v := strings.TrimSpace(rest)
		if v == "" {
			return fmt.Errorf("%s requires a value", instr.Kind)
		}
		instr.Args["value"] = v
	case InstExpose, InstVolume:
		fields := splitFields(rest)
		if len(fields) == 0 {
			return fmt.Errorf("%s requires at least one argument", instr.Kind)
		}
		instr.Args["list"] = fields
	}
	return nil
}

func parseCopyArgs(rest string) (*CopyArgs, error) {
	fields := splitFields(rest)
	ca := &CopyArgs{}
	i := 0
	for i < len(fields) && strings.HasPrefix(fields[i], "--") {
		name, val, err := splitFlag(fields[i])
		if err != nil {
			return nil, err
		}
		switch name {
		case "from":
			ca.From = val
		case "chmod":
			ca.Chmod = val
		case "chown":
			ca.Chown = val
		default:
			return nil, fmt.Errorf("COPY/ADD: unknown flag --%s", name)
		}
		i++
	}
	pos := fields[i:]
	if len(pos) < 2 {
		return nil, fmt.Errorf("COPY/ADD: requires at least a source and a destination")
	}
	ca.Sources = pos[:len(pos)-1]
	ca.Dest = pos[len(pos)-1]
	return ca, nil
}

func parseRunArgs(rest string) (*RunArgs, error) {
	ra := &RunArgs{}
	t := strings.TrimSpace(rest)
	if strings.HasPrefix(t, "[") {
		var args []string
		if err := json.Unmarshal([]byte(t), &args); err != nil {
			return nil, fmt.Errorf("exec form JSON parse error: %w", err)
		}
		if len(args) == 0 {
			return nil, fmt.Errorf("exec form requires at least one argument")
		}
		ra.ExecForm = true
		ra.ExecArgs = args
		return ra, nil
	}
	ra.Command = t
	if strings.TrimSpace(t) == "" {
		return nil, fmt.Errorf("RUN requires a command")
	}
	return ra, nil
}

func parseKVArgs(kind InstructionKind, rest string) (*KeyValueArgs, error) {
	kv := &KeyValueArgs{Pairs: map[string]string{}}
	t := strings.TrimSpace(rest)
	// Legacy form: ENV KEY VALUE (no '=' and no space-separated assignments)
	if kind == InstEnv && !strings.Contains(t, "=") {
		fields := splitFields(t)
		if len(fields) < 2 {
			return nil, fmt.Errorf("ENV legacy form requires KEY and VALUE")
		}
		k := fields[0]
		kv.Pairs[k] = strings.Join(fields[1:], " ")
		kv.Order = []string{k}
		return kv, nil
	}
	fields := splitFields(t)
	for _, f := range fields {
		eq := strings.IndexByte(f, '=')
		if eq < 0 {
			if kind == InstArg {
				kv.Pairs[f] = ""
				kv.Order = append(kv.Order, f)
				continue
			}
			return nil, fmt.Errorf("%s: expected key=value, got %q", kind, f)
		}
		k := f[:eq]
		v := f[eq+1:]
		// strip surrounding quotes
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"') {
			if uq, err := unquoteJSON(v); err == nil {
				v = uq
			}
		}
		kv.Pairs[k] = v
		kv.Order = append(kv.Order, k)
	}
	if len(kv.Order) == 0 {
		return nil, fmt.Errorf("%s requires at least one key=value", kind)
	}
	return kv, nil
}

func unquoteJSON(s string) (string, error) {
	var v string
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return "", err
	}
	return v, nil
}

func splitFlag(f string) (string, string, error) {
	body := strings.TrimPrefix(f, "--")
	if eq := strings.IndexByte(body, '='); eq >= 0 {
		return body[:eq], body[eq+1:], nil
	}
	return body, "", fmt.Errorf("flag %q requires a value", f)
}

// splitFields splits a command line into fields respecting double quotes.
func splitFields(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	started := false
	flush := func() {
		if started {
			out = append(out, cur.String())
			cur.Reset()
			started = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQuote = !inQuote
			started = true
		case c == ' ' || c == '\t':
			if inQuote {
				cur.WriteByte(c)
			} else {
				flush()
			}
		default:
			cur.WriteByte(c)
			started = true
		}
	}
	flush()
	return out
}
