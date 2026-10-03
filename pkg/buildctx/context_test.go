package buildctx

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScanDeterministicAndDigests(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"a.txt":     "hello",
		"dir/b.txt": "world",
	})
	c, err := New(root, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	e1, err := c.Scan()
	if err != nil {
		t.Fatal(err)
	}
	e2, err := c.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(e1) != len(e2) || len(e1) == 0 {
		t.Fatalf("scan mismatch: %d vs %d", len(e1), len(e2))
	}
	for i := range e1 {
		if e1[i] != e2[i] {
			t.Fatalf("entry %d differs: %+v vs %+v", i, e1[i], e2[i])
		}
	}
	// Digest must match the content.
	for _, e := range e1 {
		if e.Path == "a.txt" {
			want := "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
			if e.Digest != want {
				t.Fatalf("digest of a.txt: got %s want %s", e.Digest, want)
			}
		}
	}
	// Sorted order.
	for i := 1; i < len(e1); i++ {
		if e1[i-1].Path >= e1[i].Path {
			t.Fatalf("entries not sorted: %q >= %q", e1[i-1].Path, e1[i].Path)
		}
	}
}

func TestIgnoreRules(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"keep.txt":        "k",
		"secret.key":      "s",
		"build/out.bin":   "b",
		"build/keep.go":   "g",
		"logs/a.log":      "l",
		"logs/nested/b.log": "n",
		"sub/secret.key":  "s2",
	})
	writeFiles(t, root, map[string]string{
		".cibignore": "secret.key\nbuild/\n*.log\n!logs/nested/\n",
	})
	c, err := New(root, ".cibignore", nil)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := c.Scan()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Path] = true
	}
	for _, want := range []string{"keep.txt", "logs", "logs/nested"} {
		if !got[want] {
			t.Fatalf("expected %s to be present, entries: %v", want, got)
		}
	}
	for _, notWant := range []string{"secret.key", "sub/secret.key", "build", "build/out.bin", "logs/a.log", ".cibignore"} {
		if got[notWant] {
			t.Fatalf("expected %s to be ignored, entries: %v", notWant, got)
		}
	}
	// Stat honours ignore rules.
	if _, err := c.Stat("secret.key"); !IsNotExist(err) {
		t.Fatalf("stat ignored file: want NotExist, got %v", err)
	}
	if _, err := c.Stat("keep.txt"); err != nil {
		t.Fatalf("stat keep.txt: %v", err)
	}
	if _, err := c.Stat("missing.txt"); !IsNotExist(err) {
		t.Fatalf("stat missing: want NotExist, got %v", err)
	}
	if _, err := c.Stat("../escape"); err == nil {
		t.Fatalf("stat escaping path must fail")
	}
}

func TestExtraPatternsAndNegation(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"a.tmp": "1",
		"b.txt": "2",
	})
	c, err := New(root, "", []string{"*.tmp"})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := c.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "b.txt" {
		t.Fatalf("extra pattern not applied: %+v", entries)
	}
}
