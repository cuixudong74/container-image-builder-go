package description

import (
	"strings"
	"testing"
)

func TestParseBasic(t *testing.T) {
	df := `FROM scratch AS base
WORKDIR /app
COPY a.txt /app/
ENV FOO=bar
LABEL k=v
USER nobody
CMD ["/bin/sh"]
`
	desc, err := NewParser("Dockerfile").Parse(df)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(desc.Stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(desc.Stages))
	}
	st := desc.Stages[0]
	if st.Name != "base" {
		t.Errorf("stage name = %q, want base", st.Name)
	}
	if !st.FromScratch {
		t.Error("expected FromScratch")
	}
	if len(st.Instructions) != 6 {
		t.Fatalf("expected 6 instructions, got %d", len(st.Instructions))
	}
	kinds := []InstructionKind{InstWorkdir, InstCopy, InstEnv, InstLabel, InstUser, InstCmd}
	for i, k := range kinds {
		if st.Instructions[i].Kind != k {
			t.Errorf("instruction %d = %s, want %s", i, st.Instructions[i].Kind, k)
		}
	}
}

func TestParseMultiStage(t *testing.T) {
	df := `FROM scratch AS builder
COPY src /src
RUN ["/bin/true"]

FROM scratch AS final
COPY --from=builder /src /dst
`
	desc, err := NewParser("Dockerfile").Parse(df)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(desc.Stages) != 2 {
		t.Fatalf("expected 2 stages, got %d", len(desc.Stages))
	}
	if desc.Stages[0].Name != "builder" || desc.Stages[1].Name != "final" {
		t.Errorf("stage names = %q, %q", desc.Stages[0].Name, desc.Stages[1].Name)
	}
	ca := desc.Stages[1].Instructions[0].Args["copy"].(*CopyArgs)
	if ca.From != "builder" {
		t.Errorf("COPY --from = %q, want builder", ca.From)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name string
		df   string
		want string
	}{
		{"no FROM", "RUN echo hi\n", "must follow FROM"},
		{"unknown instruction", "FROM scratch\nBOGUS\n", "unknown instruction"},
		{"COPY no args", "FROM scratch\nCOPY\n", "requires at least"},
		{"FROM no image", "FROM\n", "missing image"},
		{"FROM bad AS", "FROM scratch AS\n", "AS requires"},
		{"RUN empty", "FROM scratch\nRUN\n", "requires a command"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewParser("Dockerfile").Parse(tc.df)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestParseLineContinuation(t *testing.T) {
	df := "FROM scratch\nCOPY a.txt \\\n  b.txt \\\n  /dst/\n"
	desc, err := NewParser("Dockerfile").Parse(df)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ca := desc.Stages[0].Instructions[0].Args["copy"].(*CopyArgs)
	if len(ca.Sources) != 2 {
		t.Fatalf("expected 2 sources, got %d", len(ca.Sources))
	}
	if ca.Dest != "/dst/" {
		t.Errorf("dest = %q, want /dst/", ca.Dest)
	}
}

func TestParseExecForm(t *testing.T) {
	df := `FROM scratch
CMD ["/bin/sh", "-c", "echo hi"]
`
	desc, err := NewParser("Dockerfile").Parse(df)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ra := desc.Stages[0].Instructions[0].Args["run"].(*RunArgs)
	if !ra.ExecForm {
		t.Error("expected exec form")
	}
	if len(ra.ExecArgs) != 3 {
		t.Errorf("exec args = %v", ra.ExecArgs)
	}
}

func TestParseGlobalARG(t *testing.T) {
	df := `ARG VERSION=1.0
FROM scratch
RUN echo $VERSION
`
	desc, err := NewParser("Dockerfile").Parse(df)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(desc.GlobalARGs) != 1 {
		t.Fatalf("expected 1 global ARG, got %d", len(desc.GlobalARGs))
	}
	if desc.GlobalARGs[0].Pairs["VERSION"] != "1.0" {
		t.Errorf("VERSION = %q", desc.GlobalARGs[0].Pairs["VERSION"])
	}
}

func TestParseComments(t *testing.T) {
	df := `# this is a comment
FROM scratch
# another comment
WORKDIR /app
`
	desc, err := NewParser("Dockerfile").Parse(df)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(desc.Stages[0].Instructions) != 1 {
		t.Errorf("expected 1 instruction, got %d", len(desc.Stages[0].Instructions))
	}
}
