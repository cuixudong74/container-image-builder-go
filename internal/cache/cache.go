// Package cache implements a content-addressed, concurrency-safe layer cache.
//
// Layout under the cache directory:
//
//	blobs/sha256/<hex>        layer tar blobs (addressed by layer digest)
//	entries/sha256/<hex>      entry metadata (addressed by cache key)
//	locks/<hex>               per-key flock targets
//	tmp/                      staging area for atomic writes
//
// All writes go through temp file + fsync + rename, so a crash or
// cancellation never exposes a half-written entry.
package cache

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/cuixudong74/container-image-builder-go/internal/digest"
)

// Meta is the metadata stored alongside a cache entry.
type Meta struct {
	// LayerDigest is the digest of the cached layer blob (empty for no-layer entries).
	LayerDigest digest.Digest `json:"layerDigest,omitempty"`
	// Size is the uncompressed layer size in bytes.
	Size int64 `json:"size,omitempty"`
	// CreatedAt is the fixed build epoch used for the layer tar.
	Epoch int64 `json:"epoch"`
	// Note is a human-readable hint of what produced the entry.
	Note string `json:"note,omitempty"`
}

// Entry is a resolved cache entry.
type Entry struct {
	Key  digest.Digest
	Meta Meta
}

// Cache is an on-disk content-addressed layer cache.
type Cache struct {
	dir     string
	blobs   string
	entries string
	locks   string
	tmp     string

	procMu sync.Mutex
	proc   map[string]*sync.Mutex // per-key in-process locks
}

// Open opens (and if needed initializes) the cache at dir.
func Open(dir string) (*Cache, error) {
	if dir == "" {
		dir = ".build-cache"
	}
	c := &Cache{
		dir:     dir,
		blobs:   filepath.Join(dir, "blobs", "sha256"),
		entries: filepath.Join(dir, "entries", "sha256"),
		locks:   filepath.Join(dir, "locks"),
		tmp:     filepath.Join(dir, "tmp"),
		proc:    map[string]*sync.Mutex{},
	}
	for _, d := range []string{c.blobs, c.entries, c.locks, c.tmp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("init cache dir %q: %w", d, err)
		}
	}
	return c, nil
}

// Dir returns the cache root directory.
func (c *Cache) Dir() string { return c.dir }

func keyHex(key digest.Digest) string { return key.Hex() }

// EntryPath returns the metadata file path for a cache key.
func (c *Cache) EntryPath(key digest.Digest) string {
	return filepath.Join(c.entries, keyHex(key))
}

// BlobPath returns the layer blob path for a layer digest.
func (c *Cache) BlobPath(layer digest.Digest) string {
	return filepath.Join(c.blobs, layer.Hex())
}

// Get looks up a cache entry. Returns (entry, true) on hit.
func (c *Cache) Get(key digest.Digest) (*Entry, bool, error) {
	p := c.EntryPath(key)
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		// A corrupt entry must never be served.
		return nil, false, fmt.Errorf("cache entry %s is corrupt: %w", key, err)
	}
	if m.LayerDigest != "" {
		if fi, err := os.Stat(c.BlobPath(m.LayerDigest)); err != nil || fi.Size() != m.Size {
			return nil, false, nil
		}
	}
	return &Entry{Key: key, Meta: m}, true, nil
}

// Acquire takes the in-process and cross-process lock for a cache key.
// The returned unlock function must be called when done.
func (c *Cache) Acquire(key digest.Digest) (unlock func(), err error) {
	hex := keyHex(key)
	c.procMu.Lock()
	mu, ok := c.proc[hex]
	if !ok {
		mu = &sync.Mutex{}
		c.proc[hex] = mu
	}
	c.procMu.Unlock()
	mu.Lock()

	lockPath := filepath.Join(c.locks, hex+".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		mu.Unlock()
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		mu.Unlock()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		mu.Unlock()
	}, nil
}

// Put atomically installs a layer blob and its metadata under key.
// layerPath is a path to the freshly built layer tar (moved into the cache).
// If the same layer already exists it is deduplicated by content.
func (c *Cache) Put(key digest.Digest, layerPath string, meta Meta) error {
	unlock, err := c.Acquire(key)
	if err != nil {
		return err
	}
	defer unlock()

	// Another builder may have populated the entry meanwhile.
	if _, hit, _ := c.Get(key); hit {
		if layerPath != "" {
			os.Remove(layerPath)
		}
		return nil
	}

	if layerPath != "" && meta.LayerDigest != "" {
		blobDst := c.BlobPath(meta.LayerDigest)
		if _, err := os.Stat(blobDst); os.IsNotExist(err) {
			if err := atomicMove(layerPath, blobDst); err != nil {
				return err
			}
		} else {
			os.Remove(layerPath)
		}
	}

	data, err := json.MarshalIndent(&meta, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWriteFile(c.EntryPath(key), data, c.tmp)
}

// atomicWriteFile writes data to a temp file in tmpDir, fsyncs and renames
// it into finalPath. The rename is atomic on the same filesystem.
func atomicWriteFile(finalPath string, data []byte, tmpDir string) error {
	tmp, err := os.CreateTemp(tmpDir, "entry-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, finalPath)
}

// atomicMove moves src to dst via a temp name + rename, fsyncing the dir.
func atomicMove(src, dst string) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), "blob-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpName)
	if err := os.Remove(tmpName); err != nil {
		return err
	}
	if err := os.Link(src, tmpName); err != nil {
		// cross-device fallback: copy
		if err := copyFile(src, tmpName); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	os.Remove(src)
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// TmpDir returns the staging directory for building layers.
func (c *Cache) TmpDir() string { return c.tmp }
