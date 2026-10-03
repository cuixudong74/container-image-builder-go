// Package description parses build description files (Dockerfile-like).
package description

// InstructionKind enumerates supported instruction types.
type InstructionKind int

const (
	InstFrom InstructionKind = iota
	InstRun
	InstCmd
	InstEntrypoint
	InstCopy
	InstAdd
	InstEnv
	InstLabel
	InstWorkdir
	InstUser
	InstArg
	InstExpose
	InstVolume
	InstMaintainer
)

// String returns the canonical instruction keyword.
func (k InstructionKind) String() string {
	switch k {
	case InstFrom:
		return "FROM"
	case InstRun:
		return "RUN"
	case InstCmd:
		return "CMD"
	case InstEntrypoint:
		return "ENTRYPOINT"
	case InstCopy:
		return "COPY"
	case InstAdd:
		return "ADD"
	case InstEnv:
		return "ENV"
	case InstLabel:
		return "LABEL"
	case InstWorkdir:
		return "WORKDIR"
	case InstUser:
		return "USER"
	case InstArg:
		return "ARG"
	case InstExpose:
		return "EXPOSE"
	case InstVolume:
		return "VOLUME"
	case InstMaintainer:
		return "MAINTAINER"
	}
	return "UNKNOWN"
}

// Instruction is a single parsed instruction with source location.
type Instruction struct {
	Kind InstructionKind
	// Line is the 1-based line number where the instruction starts.
	Line int
	// Raw is the raw (unexpanded, joined) instruction text.
	Raw string
	// Args holds parsed arguments (form depends on Kind).
	Args map[string]interface{}
}

// Stage is one FROM ... AS ... block.
type Stage struct {
	// Name is the stage name (empty for unnamed stages).
	Name string
	// Index is the 0-based stage order.
	Index int
	// BaseImage is the FROM image reference ("scratch" or a base name).
	BaseImage string
	// Platform is the optional --platform value (empty = target platform).
	Platform string
	// Instructions in order.
	Instructions []*Instruction
	// FromScratch is true when BaseImage == "scratch".
	FromScratch bool
}

// BuildDescription is the fully parsed build file.
type BuildDescription struct {
	// Stages in declaration order.
	Stages []Stage
	// SourceFile is the path of the parsed description file.
	SourceFile string
	// GlobalARGs are ARG declarations appearing before the first FROM.
	GlobalARGs []KeyValueArgs
}

// CopyArgs holds parsed COPY/ADD arguments.
type CopyArgs struct {
	Sources []string
	Dest    string
	From    string // --from stage name (empty = context)
	Chmod   string // --chmod (empty = default canonical mode)
	Chown   string // --chown (accepted, ignored for modes)
}

// RunArgs holds parsed RUN arguments.
type RunArgs struct {
	Command string
	// ExecForm is true when the instruction uses the JSON exec form.
	ExecForm bool
	ExecArgs []string
}

// KeyValueArgs holds ENV/LABEL/ARG key-value pairs.
type KeyValueArgs struct {
	Pairs map[string]string
	Order []string
}

// FromArgs holds parsed FROM arguments.
type FromArgs struct {
	Image    string
	AsName   string
	Platform string
}
