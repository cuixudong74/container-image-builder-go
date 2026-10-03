package description

import (
	"fmt"
)

// ParseError is a locatable description-file parse error.
type ParseError struct {
	File    string
	Line    int
	Message string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("%s:%d: %s", e.File, e.Line, e.Message)
}

// NewParseError constructs a ParseError.
func NewParseError(file string, line int, format string, args ...interface{}) *ParseError {
	return &ParseError{File: file, Line: line, Message: fmt.Sprintf(format, args...)}
}

// StageError is an error tied to a specific stage/instruction step.
type StageError struct {
	Stage    string
	StageIdx int
	Line     int
	Inst     string
	Message  string
}

func (e *StageError) Error() string {
	loc := ""
	if e.Stage != "" {
		loc = fmt.Sprintf("stage %q", e.Stage)
	} else {
		loc = fmt.Sprintf("stage #%d", e.StageIdx)
	}
	if e.Line > 0 {
		loc += fmt.Sprintf(", line %d", e.Line)
	}
	if e.Inst != "" {
		loc += fmt.Sprintf(" (%s)", e.Inst)
	}
	return fmt.Sprintf("%s: %s", loc, e.Message)
}

// NewStageError constructs a StageError.
func NewStageError(stage string, idx, line int, inst, format string, args ...interface{}) *StageError {
	return &StageError{
		Stage: stage, StageIdx: idx, Line: line, Inst: inst,
		Message: fmt.Sprintf(format, args...),
	}
}
