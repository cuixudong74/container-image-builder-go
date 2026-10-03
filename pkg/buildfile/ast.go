// Package buildfile parses container build description files into an AST.
package buildfile

// Kind identifies an instruction type.
type Kind int

const (
	KindFrom Kind = iota
	KindCopy
	KindEnv
	KindLabel
	KindWorkdir
	KindCmd
	KindEntrypoint
	KindUser
	KindExpose
)

func (k Kind) String() string {
	switch k {
	case KindFrom:
		return "FROM"
	case KindCopy:
		return "COPY"
	case KindEnv:
		return "ENV"
	case KindLabel:
		return "LABEL"
	case KindWorkdir:
		return "WORKDIR"
	case KindCmd:
		return "CMD"
	case KindEntrypoint:
		return "ENTRYPOINT"
	case KindUser:
		return "USER"
	case KindExpose:
		return "EXPOSE"
	}
	return "UNKNOWN"
}

// KV is an ordered key/value pair used by ENV and LABEL.
type KV struct {
	Key   string
	Value string
}

// Instruction is a single parsed build step. Line is 1-based and points at
// the physical line where the instruction starts, for error reporting.
type Instruction struct {
	Kind Kind
	Line int

	// FROM
	FromRef string // "scratch", a stage name, or "oci:<dir>"
	FromAs  string // stage alias given by AS

	// COPY
	CopyFrom  string   // source stage name, empty means build context
	CopySrcs  []string // source paths (relative to context or stage rootfs)
	CopyDest  string   // destination path inside the image
	CopyChmod string   // optional --chmod value, empty if unset

	// ENV / LABEL
	Pairs []KV

	// WORKDIR / USER
	Value string

	// CMD / ENTRYPOINT
	Argv      []string // JSON array form
	ShellForm string   // shell form (raw string), mutually exclusive with Argv

	// EXPOSE
	Ports []string
}

// Stage is one build stage: a FROM plus its instructions.
type Stage struct {
	Name   string // alias from AS, or generated "stage-N"
	Index  int
	From   Instruction
	Body   []Instruction
	Line   int // line of the FROM instruction
}

// Buildfile is the parsed result.
type Buildfile struct {
	Stages []*Stage
	byName map[string]*Stage
}

// StageByName returns the stage with the given alias, or nil.
func (b *Buildfile) StageByName(name string) *Stage { return b.byName[name] }

// ParseError is a parse/validation failure with a source location.
type ParseError struct {
	Line int    // 0 means file-level error
	Msg  string
}

func (e *ParseError) Error() string {
	if e.Line > 0 {
		return "buildfile: line " + itoa(e.Line) + ": " + e.Msg
	}
	return "buildfile: " + e.Msg
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	p := len(buf)
	for i > 0 {
		p--
		buf[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		buf[p] = '-'
	}
	return string(buf[p:])
}
