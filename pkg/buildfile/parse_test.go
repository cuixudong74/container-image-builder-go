package buildfile

import (
	"strings"
	"testing"
)

func TestParseMultiStage(t *testing.T) {
	src := `
# comment
FROM scratch AS base
COPY app /app
ENV A=1 B="two words"

FROM base AS final
COPY --from=base /app/bin /bin
CMD ["/bin/app"]
EXPOSE 8080 9090/udp
`
	bf, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(bf.Stages) != 2 {
		t.Fatalf("want 2 stages, got %d", len(bf.Stages))
	}
	if bf.Stages[0].Name != "base" || bf.Stages[1].Name != "final" {
		t.Fatalf("stage names: %q %q", bf.Stages[0].Name, bf.Stages[1].Name)
	}
	env := bf.Stages[0].Body[1]
	if env.Kind != KindEnv || len(env.Pairs) != 2 || env.Pairs[1].Value != "two words" {
		t.Fatalf("env pairs: %+v", env.Pairs)
	}
	cp := bf.Stages[1].Body[0]
	if cp.CopyFrom != "base" || cp.CopySrcs[0] != "/app/bin" || cp.CopyDest != "/bin" {
		t.Fatalf("copy: %+v", cp)
	}
	cmd := bf.Stages[1].Body[1]
	if len(cmd.Argv) != 1 || cmd.Argv[0] != "/bin/app" {
		t.Fatalf("cmd: %+v", cmd)
	}
	ex := bf.Stages[1].Body[2]
	if len(ex.Ports) != 2 || ex.Ports[1] != "9090/udp" {
		t.Fatalf("expose: %+v", ex)
	}
}

func TestParseContinuation(t *testing.T) {
	src := "FROM scratch\nENV A=1 \\\n    B=2\n"
	bf, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(bf.Stages[0].Body) != 1 || len(bf.Stages[0].Body[0].Pairs) != 2 {
		t.Fatalf("continuation not joined: %+v", bf.Stages[0].Body)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name, src, want string
		line            int
	}{
		{"no from", "COPY a /b\n", "before any FROM", 1},
		{"unknown insn", "FROM scratch\nFROBNICATE x\n", "unknown instruction", 2},
		{"empty", "# nothing\n", "no FROM", 0},
		{"unknown stage", "FROM ghost\n", "unknown stage", 1},
		{"forward stage", "FROM later\nFROM scratch AS later\n", "defined later", 1},
		{"copy from ghost", "FROM scratch\nCOPY --from=ghost /a /b\n", "unknown stage", 2},
		{"copy from later", "FROM scratch AS a\nCOPY --from=b /x /y\nFROM scratch AS b\n", "defined later", 2},
		{"bad env", "FROM scratch\nENV NOPE\n", "KEY=VALUE", 2},
		{"bad cmd json", "FROM scratch\nCMD [unclosed\n", "bad JSON", 2},
		{"copy one arg", "FROM scratch\nCOPY onlyone\n", "destination", 2},
		{"dup stage", "FROM scratch AS a\nFROM scratch AS a\n", "duplicate stage", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tc.src))
			if err == nil {
				t.Fatalf("want error containing %q", tc.want)
			}
			pe, ok := err.(*ParseError)
			if !ok {
				t.Fatalf("want *ParseError, got %T (%v)", err, err)
			}
			if !strings.Contains(pe.Msg, tc.want) {
				t.Fatalf("error %q does not contain %q", pe.Msg, tc.want)
			}
			if pe.Line != tc.line {
				t.Fatalf("want line %d, got %d", tc.line, pe.Line)
			}
		})
	}
}

func TestDefaultStageNames(t *testing.T) {
	bf, err := Parse(strings.NewReader("FROM scratch\nFROM scratch\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if bf.Stages[0].Name != "stage-0" || bf.Stages[1].Name != "stage-1" {
		t.Fatalf("default names: %q %q", bf.Stages[0].Name, bf.Stages[1].Name)
	}
}
