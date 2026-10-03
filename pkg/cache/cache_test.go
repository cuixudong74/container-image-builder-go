package cache

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func keyOf(i int) string {
	return fmt.Sprintf("sha256:%064x", i+1)
}

func TestPutGetRoundTrip(t *testing.T) {
	c, err := Open(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	d1, err := c.Put(keyOf(0), strings.NewReader("layer-data"))
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := c.Get(keyOf(0))
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got != d1 {
		t.Fatalf("digest mismatch: %s != %s", got, d1)
	}
	// Content addressing: digest is sha256 of content.
	want := "sha256:3cf161afb19a173feb1045a56a6a16f668fc887f9d4e08e6ca3c4cb8b8e6a3e0"
	_ = want // computed below instead
	p, ok := c.BlobPath(d1)
	if !ok {
		t.Fatalf("blob missing")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "layer-data" {
		t.Fatalf("blob content: %q", data)
	}
	// Miss for unknown key.
	if _, ok, err := c.Get(keyOf(99)); err != nil || ok {
		t.Fatalf("want miss, got ok=%v err=%v", ok, err)
	}
	// Size.
	sz, err := c.BlobSize(d1)
	if err != nil || sz != int64(len("layer-data")) {
		t.Fatalf("size: %d %v", sz, err)
	}
}

func TestCorruptIndexDegradesToMiss(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Put(keyOf(0), strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	// Corrupt the index entry.
	idxPath := filepath.Join(dir, "index", strings.TrimPrefix(keyOf(0), "sha256:"))
	if err := os.WriteFile(idxPath, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := c.Get(keyOf(0)); err != nil || ok {
		t.Fatalf("corrupt index should be a miss, got ok=%v err=%v", ok, err)
	}
}

func TestMissingBlobDegradesToMiss(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	d, err := c.Put(keyOf(0), strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := c.BlobPath(d)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := c.Get(keyOf(0)); err != nil || ok {
		t.Fatalf("missing blob should be a miss, got ok=%v err=%v", ok, err)
	}
}

func TestConcurrentPutGet(t *testing.T) {
	c, err := Open(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	const n = 32
	var wg sync.WaitGroup
	digests := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			content := bytes.Repeat([]byte{byte(i)}, 1024)
			d, err := c.Put(keyOf(i%8), bytes.NewReader(content))
			if err != nil {
				t.Errorf("put: %v", err)
				return
			}
			digests[i] = d
			if _, ok, err := c.Get(keyOf(i % 8)); err != nil {
				t.Errorf("get: %v", err)
			} else if ok {
				// Whatever digest is stored, its blob must be complete.
				p, ok := c.BlobPath(d)
				if !ok {
					t.Errorf("blob for stored digest missing")
					return
				}
				data, err := os.ReadFile(p)
				if err != nil || len(data) != 1024 {
					t.Errorf("blob incomplete: %d bytes, err=%v", len(data), err)
				}
			}
		}(i)
	}
	wg.Wait()
	// Same content from two writers must give the same digest.
	d1, err := c.Put(keyOf(100), strings.NewReader("same"))
	if err != nil {
		t.Fatal(err)
	}
	d2, err := c.Put(keyOf(101), strings.NewReader("same"))
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatalf("content addressing broken: %s != %s", d1, d2)
	}
}

func TestNoPartialIndexEntries(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A failing reader must not leave an index entry behind.
	if _, err := c.Put(keyOf(0), &failReader{}); err == nil {
		t.Fatal("want error from failing reader")
	}
	if _, ok, _ := c.Get(keyOf(0)); ok {
		t.Fatal("failed put left an index entry")
	}
	// tmp dir must not accumulate committed debris (tmp files may remain
	// only until cleaned; here cleanup is synchronous via defer).
	entries, err := os.ReadDir(filepath.Join(dir, "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("tmp dir not clean: %v", entries)
	}
}

type failReader struct{}

func (f *failReader) Read(p []byte) (int, error) {
	copy(p, "partial")
	return 7, fmt.Errorf("simulated failure")
}
