package oci

import (
	"archive/tar"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"time"

	"github.com/cuixudong74/container-image-builder-go/internal/digest"
)

// LayerFileType classifies entries in a layer tar.
type LayerFileType int

const (
	LayerRegular LayerFileType = iota
	LayerDir
	LayerSymlink
)

// LayerFile is one entry to include in a layer tar.
type LayerFile struct {
	RelPath string // slash-separated path inside the image (no leading /)
	AbsPath string // source path on disk (for regular files)
	Type    LayerFileType
	Target  string // symlink target
	// Mode overrides the canonical mode when non-zero (used by COPY --chmod).
	Mode os.FileMode
}

// layerEntry is a normalized tar entry.
type layerEntry struct {
	name     string // tar header name (dirs end with /)
	Typeflag byte
	mode     int64
	linkname string
	absPath  string
	size     int64
	whiteout bool
}

// WriteLayerTar writes a deterministic, content-addressed layer tar to w.
// All metadata is canonicalized: fixed epoch, uid/gid 0, sorted paths,
// normalized permissions (dirs 0755, files 0644, symlinks 0777).
// It returns the uncompressed digest and size of the tar stream.
func WriteLayerTar(w io.Writer, epoch time.Time, files []LayerFile, deletions []string) (digest.Digest, int64, error) {
	entries, err := normalizeEntries(files, deletions)
	if err != nil {
		return "", 0, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	h := sha256.New()
	cw := &countingWriter{w: w}
	tw := tar.NewWriter(io.MultiWriter(h, cw))

	for _, e := range entries {
		hdr := &tar.Header{
			Name:       e.name,
			Typeflag:   e.Typeflag,
			Mode:       e.mode,
			Uid:        0,
			Gid:        0,
			Uname:      "",
			Gname:      "",
			ModTime:    epoch,
			AccessTime: epoch,
			ChangeTime: epoch,
		}
		if e.Typeflag == tar.TypeSymlink {
			hdr.Linkname = e.linkname
		}
		if e.Typeflag == tar.TypeReg {
			hdr.Size = e.size
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return "", 0, fmt.Errorf("write tar header for %q: %w", e.name, err)
		}
		if e.Typeflag == tar.TypeReg && !e.whiteout {
			f, err := os.Open(e.absPath)
			if err != nil {
				return "", 0, fmt.Errorf("open source for %q: %w", e.name, err)
			}
			if _, err := io.Copy(tw, f); err != nil {
				f.Close()
				return "", 0, fmt.Errorf("write tar content for %q: %w", e.name, err)
			}
			f.Close()
		}
	}
	if err := tw.Close(); err != nil {
		return "", 0, err
	}
	return digest.Digest("sha256:" + fmt.Sprintf("%x", h.Sum(nil))), cw.n, nil
}

// normalizeEntries builds the full entry set including implicit parent
// directories and whiteout entries for deletions.
func normalizeEntries(files []LayerFile, deletions []string) ([]layerEntry, error) {
	seen := map[string]bool{}
	var out []layerEntry

	addDir := func(rel string) {
		rel = path.Clean(rel)
		if rel == "." || rel == "/" {
			return
		}
		name := rel + "/"
		if seen[name] {
			return
		}
		seen[name] = true
		out = append(out, layerEntry{name: name, Typeflag: tar.TypeDir, mode: 0o755})
	}

	// Explicit directories and parent dirs of files.
	for _, f := range files {
		rel := path.Clean(f.RelPath)
		if rel == "." || rel == "/" {
			continue
		}
		// parent directories
		parent := path.Dir(rel)
		for parent != "." && parent != "/" {
			addDir(parent)
			parent = path.Dir(parent)
		}
		switch f.Type {
		case LayerDir:
			addDir(rel)
		case LayerSymlink:
			name := rel
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, layerEntry{
				name: name, Typeflag: tar.TypeSymlink, mode: 0o777, linkname: f.Target,
			})
		default:
			info, err := os.Stat(f.AbsPath)
			if err != nil {
				return nil, fmt.Errorf("stat source %q: %w", f.AbsPath, err)
			}
			name := rel
			if seen[name] {
				continue
			}
			seen[name] = true
			mode := int64(0o644)
			if f.Mode != 0 {
				mode = int64(f.Mode.Perm())
			}
			out = append(out, layerEntry{
				name: name, Typeflag: tar.TypeReg, mode: mode,
				absPath: f.AbsPath, size: info.Size(),
			})
		}
	}

	// Whiteouts for deleted paths: .wh.<basename> in the parent directory.
	for _, d := range deletions {
		rel := path.Clean(d)
		if rel == "." || rel == "/" {
			continue
		}
		parent := path.Dir(rel)
		base := path.Base(rel)
		whName := path.Join(parent, ".wh."+base)
		if parent == "." {
			whName = ".wh." + base
		}
		if seen[whName] {
			continue
		}
		seen[whName] = true
		out = append(out, layerEntry{
			name: whName, Typeflag: tar.TypeReg, mode: 0o644, whiteout: true,
		})
		// ensure the parent dir entry exists
		if parent != "." {
			addDir(parent)
		}
	}
	return out, nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
