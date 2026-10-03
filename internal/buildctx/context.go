// Package buildctx handles the build context: walking, ignore rules and
// content-addressed tree hashing.
package buildctx

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cuixudong74/container-image-builder-go/internal/digest"
)

// EntryType classifies a context entry.
type EntryType int

const (
	EntryFile EntryType = iota
	EntryDir
	EntrySymlink
)

// Entry is one file/dir/symlink in the build context.
type Entry struct {
	RelPath string // slash-separated, relative to context root
	AbsPath string
	Type    EntryType
	Size    int64
	Target  string // symlink target (raw)
	Mode    os.FileMode
}

// Context is an opened build context with a stable content tree hash.
type Context struct {
	root     string
	ignore   *IgnoreMatcher
	entries  []Entry
	byRel    map[string]*Entry
	treeHash digest.Digest
}

// Options controls context walking.
type Options struct {
	// MaxFileSize rejects individual context files larger than this (0 = unlimited).
	MaxFileSize int64
}

// Open walks root, applies ignore rules and computes the content tree hash.
func Open(root string, opts Options) (*Context, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve context path %q: %w", root, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("open build context %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("build context %q is not a directory", root)
	}
	ig, err := LoadIgnore(abs)
	if err != nil {
		return nil, err
	}
	c := &Context{root: abs, ignore: ig, byRel: map[string]*Entry{}}
	err = filepath.Walk(abs, func(path string, fi os.FileInfo, werr error) error {
		if werr != nil {
			return fmt.Errorf("walk context: %w", werr)
		}
		rel, rerr := filepath.Rel(abs, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		isDir := fi.IsDir()
		if ig.Match(rel, isDir) {
			if isDir {
				return filepath.SkipDir
			}
			return nil
		}
		// never let .dockerignore itself leak into the image
		if rel == ".dockerignore" {
			if isDir {
				return filepath.SkipDir
			}
			return nil
		}
		e := Entry{RelPath: rel, AbsPath: path, Mode: fi.Mode()}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			e.Type = EntrySymlink
			tgt, lerr := os.Readlink(path)
			if lerr != nil {
				return lerr
			}
			e.Target = tgt
		case isDir:
			e.Type = EntryDir
		default:
			e.Type = EntryFile
			e.Size = fi.Size()
			if opts.MaxFileSize > 0 && e.Size > opts.MaxFileSize {
				return fmt.Errorf("context file %q is too large: %d bytes (limit %d)", rel, e.Size, opts.MaxFileSize)
			}
		}
		c.entries = append(c.entries, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(c.entries, func(i, j int) bool { return c.entries[i].RelPath < c.entries[j].RelPath })
	for i := range c.entries {
		c.byRel[c.entries[i].RelPath] = &c.entries[i]
	}
	h, err := c.computeTreeHash()
	if err != nil {
		return nil, err
	}
	c.treeHash = h
	return c, nil
}

// Root returns the absolute context root.
func (c *Context) Root() string { return c.root }

// Entries returns all non-ignored entries, sorted by path.
func (c *Context) Entries() []Entry { return c.entries }

// TreeDigest returns the content-addressed tree digest.
func (c *Context) TreeDigest() digest.Digest { return c.treeHash }

// Stat returns the entry for rel (exact match).
func (c *Context) Stat(rel string) (Entry, bool) {
	rel = filepath.ToSlash(strings.TrimPrefix(rel, "./"))
	e, ok := c.byRel[rel]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

// OpenFile opens the context file at rel.
func (c *Context) OpenFile(rel string) (io.ReadCloser, error) {
	e, ok := c.byRel[filepath.ToSlash(strings.TrimPrefix(rel, "./"))]
	if !ok {
		return nil, fmt.Errorf("context file %q not found", rel)
	}
	if e.Type != EntryFile {
		return nil, fmt.Errorf("context file %q is not a regular file", rel)
	}
	return os.Open(e.AbsPath)
}

// ResolveSources resolves COPY source patterns (which may contain wildcards)
// to concrete entries. Patterns match against slash-separated relative paths.
func (c *Context) ResolveSources(patterns []string) ([]Entry, error) {
	var out []Entry
	seen := map[string]bool{}
	for _, pat := range patterns {
		pat = filepath.ToSlash(pat)
		pat = strings.TrimPrefix(pat, "./")
		if pat == "." || pat == "" {
			// "." means every file in the context root.
			for _, e := range c.entries {
				if e.Type != EntryDir && !seen[e.RelPath] {
					seen[e.RelPath] = true
					out = append(out, e)
				}
			}
			continue
		}
		if strings.ContainsAny(pat, "*?[") {
			matched := false
			for _, e := range c.entries {
				if e.Type != EntryFile && e.Type != EntrySymlink {
					continue
				}
				ok, merr := filepath.Match(pat, e.RelPath)
				if merr != nil {
					return nil, fmt.Errorf("invalid source pattern %q: %w", pat, merr)
				}
				if ok && !seen[e.RelPath] {
					seen[e.RelPath] = true
					out = append(out, e)
					matched = true
				}
			}
			if !matched {
				return nil, fmt.Errorf("no files matching %q in build context", pat)
			}
		} else {
			e, ok := c.byRel[pat]
			if !ok {
				return nil, fmt.Errorf("context file %q not found", pat)
			}
			if e.Type == EntryDir {
				// directory source: include all files beneath it
				prefix := pat + "/"
				for _, sub := range c.entries {
					if strings.HasPrefix(sub.RelPath, prefix) && sub.Type != EntryDir && !seen[sub.RelPath] {
						seen[sub.RelPath] = true
						out = append(out, sub)
					}
				}
			} else {
				if !seen[e.RelPath] {
					seen[e.RelPath] = true
					out = append(out, *e)
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelPath < out[j].RelPath })
	return out, nil
}

func (c *Context) computeTreeHash() (digest.Digest, error) {
	h := sha256.New()
	var buf [8]byte
	for _, e := range c.entries {
		// type tag
		switch e.Type {
		case EntryFile:
			h.Write([]byte("f"))
		case EntryDir:
			h.Write([]byte("d"))
		case EntrySymlink:
			h.Write([]byte("l"))
		}
		writeLen := func(s string) {
			n := len(s)
			for i := 0; i < 8; i++ {
				buf[i] = byte(n >> (uint(i) * 8))
			}
			h.Write(buf[:])
		}
		writeLen(e.RelPath)
		h.Write([]byte(e.RelPath))
		switch e.Type {
		case EntryFile:
			f, err := os.Open(e.AbsPath)
			if err != nil {
				return "", err
			}
			fh := sha256.New()
			if _, err := io.Copy(fh, f); err != nil {
				f.Close()
				return "", err
			}
			f.Close()
			var sum [32]byte
			copy(sum[:], fh.Sum(nil))
			h.Write(sum[:])
		case EntrySymlink:
			writeLen(e.Target)
			h.Write([]byte(e.Target))
		}
	}
	return digest.Digest("sha256:" + fmt.Sprintf("%x", h.Sum(nil))), nil
}
