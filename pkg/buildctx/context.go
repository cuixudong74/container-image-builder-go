// Package buildctx scans the build context directory, applies ignore rules
// and computes per-file content digests for change detection.
package buildctx

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FileEntry describes one file inside the build context.
type FileEntry struct {
	Path     string      // slash-separated path relative to context root
	Mode     fs.FileMode // permission bits (and type bits for symlinks)
	Size     int64
	Digest   string // "sha256:..." of file contents, empty for dirs/symlinks
	Linkname string // target for symlinks
	IsDir    bool
}

// Context is a build context directory plus ignore rules.
type Context struct {
	Root       string
	Patterns   []string // ignore patterns (.cibignore syntax)
	IgnoreFile string   // name of the ignore file inside Root; "" disables
	m          *matcher
}

// New creates a Context. Ignore patterns are read from the ignore file in
// root when present; extra patterns are appended.
func New(root string, ignoreFile string, extra []string) (*Context, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("context dir: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("context dir: %s is not a directory", root)
	}
	var patterns []string
	if ignoreFile != "" {
		data, err := os.ReadFile(filepath.Join(root, ignoreFile))
		if err == nil {
			patterns = append(patterns, strings.Split(string(data), "\n")...)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read ignore file: %w", err)
		}
	}
	patterns = append(patterns, extra...)
	m, err := compilePatterns(patterns)
	if err != nil {
		return nil, fmt.Errorf("ignore rules: %w", err)
	}
	return &Context{Root: root, Patterns: patterns, IgnoreFile: ignoreFile, m: m}, nil
}

// Scan walks the context and returns the entries not excluded by ignore
// rules, sorted by path. The result is deterministic: identical content
// yields identical entries regardless of walk order or filesystem.
func (c *Context) Scan() ([]FileEntry, error) {
	var out []FileEntry
	err := filepath.WalkDir(c.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(c.Root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		isDir := d.IsDir()
		if c.m.Match(rel, isDir) {
			if isDir {
				return filepath.SkipDir
			}
			return nil
		}
		// The ignore file itself never enters the image.
		if rel == c.IgnoreFile {
			return nil
		}
		e, err := c.entry(rel, p, d)
		if err != nil {
			return err
		}
		out = append(out, e)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan context: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (c *Context) entry(rel, abs string, d fs.DirEntry) (FileEntry, error) {
	info, err := d.Info()
	if err != nil {
		return FileEntry{}, fmt.Errorf("stat %s: %w", rel, err)
	}
	e := FileEntry{Path: rel, Mode: info.Mode(), IsDir: info.IsDir()}
	switch {
	case info.IsDir():
	case info.Mode().IsRegular():
		e.Size = info.Size()
		dg, err := digestFile(abs)
		if err != nil {
			return FileEntry{}, err
		}
		e.Digest = dg
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(abs)
		if err != nil {
			return FileEntry{}, fmt.Errorf("readlink %s: %w", rel, err)
		}
		e.Linkname = target
	default:
		return FileEntry{}, fmt.Errorf("unsupported file type in context: %s", rel)
	}
	return e, nil
}

func digestFile(abs string) (string, error) {
	f, err := os.Open(abs)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", abs, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("digest %s: %w", abs, err)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// AbsPath returns the absolute host path of a context-relative file.
func (c *Context) AbsPath(path string) string {
	return filepath.Join(c.Root, filepath.FromSlash(path))
}

// Open opens the context-relative path for reading.
func (c *Context) Open(path string) (io.ReadCloser, error) {
	if err := checkRel(path); err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(c.Root, filepath.FromSlash(path)))
	if err != nil {
		return nil, fmt.Errorf("open context file %s: %w", path, err)
	}
	return f, nil
}

// Stat returns metadata for one context-relative path, honouring ignore
// rules: an ignored path reports not-exist.
func (c *Context) Stat(path string) (FileEntry, error) {
	if err := checkRel(path); err != nil {
		return FileEntry{}, err
	}
	if c.m.Match(path, false) || path == c.IgnoreFile {
		return FileEntry{}, &NotExistError{Path: path}
	}
	abs := filepath.Join(c.Root, filepath.FromSlash(path))
	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return FileEntry{}, &NotExistError{Path: path}
		}
		return FileEntry{}, fmt.Errorf("stat %s: %w", path, err)
	}
	e := FileEntry{Path: path, Mode: info.Mode(), IsDir: info.IsDir()}
	switch {
	case info.IsDir():
	case info.Mode().IsRegular():
		e.Size = info.Size()
		dg, err := digestFile(abs)
		if err != nil {
			return FileEntry{}, err
		}
		e.Digest = dg
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(abs)
		if err != nil {
			return FileEntry{}, fmt.Errorf("readlink %s: %w", path, err)
		}
		e.Linkname = target
	default:
		return FileEntry{}, fmt.Errorf("unsupported file type in context: %s", path)
	}
	return e, nil
}

// NotExistError reports a path missing from (or ignored in) the context.
type NotExistError struct{ Path string }

func (e *NotExistError) Error() string {
	return "no such file in build context: " + e.Path
}

// IsNotExist reports whether err is a *NotExistError.
func IsNotExist(err error) bool {
	_, ok := err.(*NotExistError)
	return ok
}

// checkRel rejects absolute and escaping paths.
func checkRel(p string) error {
	if p == "" || p == "." {
		return fmt.Errorf("invalid context path %q", p)
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
		return fmt.Errorf("context path %q escapes the context directory", p)
	}
	return nil
}
