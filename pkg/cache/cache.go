// Package cache implements a content-addressed, cross-build layer cache.
// Keys are chain keys (hash of instruction + inputs + parent key); values
// are layer blob digests. Blobs are stored by content digest. All writes
// are atomic (tmp file + rename) and guarded by per-key file locks so
// concurrent processes never observe partial entries.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// Cache is a filesystem-backed layer cache rooted at Dir.
type Cache struct {
	Dir string

	mu     sync.Mutex // guards locks map
	locks  map[string]*sync.Mutex
}

// Open creates (if needed) and returns the cache rooted at dir.
func Open(dir string) (*Cache, error) {
	for _, sub := range []string{"blobs/sha256", "index", "locks", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("cache: create %s: %w", sub, err)
		}
	}
	return &Cache{Dir: dir, locks: map[string]*sync.Mutex{}}, nil
}

func (c *Cache) keyPath(key string) (string, error) {
	k, err := sanitize(key)
	if err != nil {
		return "", err
	}
	return filepath.Join(c.Dir, "index", k), nil
}

func sanitize(key string) (string, error) {
	key = strings.TrimPrefix(key, "sha256:")
	if len(key) != 64 {
		return "", fmt.Errorf("cache: malformed key/digest (want 64 hex chars)")
	}
	for _, r := range key {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return "", fmt.Errorf("cache: malformed key/digest %q", key)
		}
	}
	return key, nil
}

// Get returns the blob digest stored for key, or ok=false on a miss.
// A hit is only reported when the referenced blob actually exists, so a
// corrupted or pruned blob degrades to a clean cache miss.
func (c *Cache) Get(key string) (digest string, ok bool, err error) {
	p, err := c.keyPath(key)
	if err != nil {
		return "", false, err
	}
	unlock := c.lockKey(key)
	defer unlock()
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("cache: read index: %w", err)
	}
	digest = strings.TrimSpace(string(data))
	if _, err := sanitize(digest); err != nil {
		// Corrupt index entry: treat as miss and remove it.
		_ = os.Remove(p)
		return "", false, nil
	}
	if _, ok := c.BlobPath(digest); !ok {
		_ = os.Remove(p)
		return "", false, nil
	}
	return digest, true, nil
}

// Put stores r as a blob (if not already present) and records key->digest.
// It returns the blob digest. Concurrent Puts of the same key are
// serialised; readers see either the old or the new entry, never a
// partial one.
func (c *Cache) Put(key string, r io.Reader) (digest string, err error) {
	if _, err := c.keyPath(key); err != nil {
		return "", err
	}
	unlock := c.lockKey(key)
	defer unlock()

	// 1. Spool blob to a tmp file while hashing.
	tmp, err := os.CreateTemp(filepath.Join(c.Dir, "tmp"), "blob-*")
	if err != nil {
		return "", fmt.Errorf("cache: tmp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), r); err != nil {
		tmp.Close()
		return "", fmt.Errorf("cache: spool blob: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("cache: fsync blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("cache: close blob: %w", err)
	}
	digest = "sha256:" + hex.EncodeToString(h.Sum(nil))

	// 2. Atomically move the blob into place (rename is atomic; if a blob
	//    with the same digest already exists it is identical by content
	//    addressing, so replacing is safe).
	blobPath := filepath.Join(c.Dir, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))
	if err := os.Rename(tmpName, blobPath); err != nil {
		return "", fmt.Errorf("cache: commit blob: %w", err)
	}

	// 3. Atomically publish the index entry.
	idx, err := c.keyPath(key)
	if err != nil {
		return "", err
	}
	itmp, err := os.CreateTemp(filepath.Join(c.Dir, "tmp"), "idx-*")
	if err != nil {
		return "", fmt.Errorf("cache: tmp index: %w", err)
	}
	itmpName := itmp.Name()
	defer os.Remove(itmpName)
	if _, err := itmp.WriteString(digest); err != nil {
		itmp.Close()
		return "", fmt.Errorf("cache: write index: %w", err)
	}
	if err := itmp.Sync(); err != nil {
		itmp.Close()
		return "", fmt.Errorf("cache: fsync index: %w", err)
	}
	if err := itmp.Close(); err != nil {
		return "", fmt.Errorf("cache: close index: %w", err)
	}
	if err := os.Rename(itmpName, idx); err != nil {
		return "", fmt.Errorf("cache: commit index: %w", err)
	}
	return digest, nil
}

// BlobPath returns the filesystem path of a blob by digest, or ok=false
// when the blob is absent.
func (c *Cache) BlobPath(digest string) (string, bool) {
	k, err := sanitize(digest)
	if err != nil {
		return "", false
	}
	p := filepath.Join(c.Dir, "blobs", "sha256", k)
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		return "", false
	}
	return p, true
}

// BlobSize returns the size of a stored blob.
func (c *Cache) BlobSize(digest string) (int64, error) {
	p, ok := c.BlobPath(digest)
	if !ok {
		return 0, fmt.Errorf("cache: blob %s not found", digest)
	}
	info, err := os.Stat(p)
	if err != nil {
		return 0, fmt.Errorf("cache: stat blob: %w", err)
	}
	return info.Size(), nil
}

// lockKey takes the in-process mutex for key and then an advisory file
// lock for cross-process exclusion. The file lock is best-effort: on
// filesystems without flock support the in-process mutex still protects
// this process.
func (c *Cache) lockKey(key string) func() {
	k, _ := sanitize(key)
	c.mu.Lock()
	m, ok := c.locks[k]
	if !ok {
		m = &sync.Mutex{}
		c.locks[k] = m
	}
	c.mu.Unlock()
	m.Lock()

	lockPath := filepath.Join(c.Dir, "locks", k+".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return m.Unlock
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return m.Unlock
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		m.Unlock()
	}
}
