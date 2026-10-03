package cache

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cuixudong74/container-image-builder-go/internal/digest"
)

func TestCachePutGet(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := digest.FromString("test-key")
	layer := filepath.Join(t.TempDir(), "layer.tar")
	if err := os.WriteFile(layer, []byte("layerdata"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := Meta{LayerDigest: digest.FromString("layerdata"), Size: 9, Epoch: 0, Note: "test"}
	if err := c.Put(key, layer, meta); err != nil {
		t.Fatal(err)
	}
	entry, hit, err := c.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if !hit {
		t.Fatal("expected cache hit")
	}
	if entry.Meta.LayerDigest != meta.LayerDigest {
		t.Errorf("layer digest = %q, want %q", entry.Meta.LayerDigest, meta.LayerDigest)
	}
}

func TestCacheMiss(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, hit, err := c.Get(digest.FromString("nonexistent"))
	if err != nil {
		t.Fatal(err)
	}
	if hit {
		t.Error("expected cache miss")
	}
}

func TestCacheConcurrentPut(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := digest.FromString("concurrent-key")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			layer := filepath.Join(t.TempDir(), "layer.tar")
			os.WriteFile(layer, []byte("samedata"), 0o644)
			meta := Meta{LayerDigest: digest.FromString("samedata"), Size: 8, Epoch: 0}
			if err := c.Put(key, layer, meta); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	entry, hit, err := c.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if !hit {
		t.Fatal("expected cache hit after concurrent puts")
	}
	if entry.Meta.Size != 8 {
		t.Errorf("size = %d", entry.Meta.Size)
	}
}

func TestCacheNoHalfWrittenEntries(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := digest.FromString("atomic-key")
	layer := filepath.Join(t.TempDir(), "layer.tar")
	os.WriteFile(layer, []byte("data"), 0o644)
	if err := c.Put(key, layer, Meta{LayerDigest: digest.FromString("data"), Size: 4}); err != nil {
		t.Fatal(err)
	}
	// No temp files should remain.
	entries, _ := os.ReadDir(c.TmpDir())
	for _, e := range entries {
		t.Errorf("temp file left behind: %s", e.Name())
	}
}
