package buildctx

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestContextTreeHashStable(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "hello")
	writeFile(t, filepath.Join(dir, "sub", "b.txt"), "world")

	c1, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	c2, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c1.TreeDigest() != c2.TreeDigest() {
		t.Error("tree hash changed across identical opens")
	}
}

func TestContextChangeDetection(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "hello")
	c1, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "a.txt"), "changed")
	c2, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c1.TreeDigest() == c2.TreeDigest() {
		t.Error("tree hash did not change after content modification")
	}
}

func TestContextIgnoreRules(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "keep")
	writeFile(t, filepath.Join(dir, "secret.txt"), "ignore")
	writeFile(t, filepath.Join(dir, "sub", "n.txt"), "keep")
	writeFile(t, filepath.Join(dir, "sub", "secret.log"), "ignore")
	writeFile(t, filepath.Join(dir, ".dockerignore"), "secret.txt\n*.log\n")

	c, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range c.Entries() {
		if e.RelPath == "secret.txt" || e.RelPath == "sub/secret.log" {
			t.Errorf("ignored file %q was included", e.RelPath)
		}
	}
	found := false
	for _, e := range c.Entries() {
		if e.RelPath == "a.txt" {
			found = true
		}
	}
	if !found {
		t.Error("a.txt was ignored but should be kept")
	}
}

func TestContextResolveSources(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "a")
	writeFile(t, filepath.Join(dir, "b.txt"), "b")
	writeFile(t, filepath.Join(dir, "sub", "c.txt"), "c")

	c, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := c.ResolveSources([]string{"*.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 matches, got %d", len(entries))
	}
	entries, err = c.ResolveSources([]string{"sub"})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 file under sub, got %d", len(entries))
	}
	_, err = c.ResolveSources([]string{"missing.txt"})
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestContextDotSource(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "a")
	writeFile(t, filepath.Join(dir, "b.txt"), "b")

	c, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := c.ResolveSources([]string{"."})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 files for '.', got %d", len(entries))
	}
}
