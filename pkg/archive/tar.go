// Package archive writes deterministic tar archives used as image layers.
// Determinism: entries are emitted in the order given (callers sort),
// uid/gid are zeroed, uname/gname are empty and mtimes are pinned to a
// fixed epoch. Identical entry lists always produce identical bytes.
package archive

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"time"
)

// Entry is one member of a layer tar.
type Entry struct {
	Name     string    // slash-separated path, no leading "./"
	Typeflag byte      // tar typeflag; zero means regular file
	Mode     int64     // permission bits
	Size     int64     // regular file size
	Linkname string    // symlink target
	ModTime  time.Time // zero means use the epoch passed to WriteTar
}

// OpenFunc opens the source file backing an Entry (regular files only).
type OpenFunc func(e Entry) (io.ReadCloser, error)

// WriteTar writes entries as a tar stream. Entries must already be sorted
// by Name. All headers are normalised for reproducibility.
func WriteTar(w io.Writer, epoch time.Time, entries []Entry, open OpenFunc) error {
	tw := tar.NewWriter(w)
	for _, e := range entries {
		if e.Name == "" {
			return fmt.Errorf("archive: entry with empty name")
		}
		mtime := e.ModTime
		if mtime.IsZero() {
			mtime = epoch
		}
		hdr := &tar.Header{
			Name:       e.Name,
			Mode:       e.Mode,
			Uid:        0,
			Gid:        0,
			Uname:      "",
			Gname:      "",
			ModTime:    mtime,
			AccessTime: time.Time{},
			ChangeTime: time.Time{},
			Typeflag:   e.Typeflag,
			Linkname:   e.Linkname,
			Format:     tar.FormatPAX,
		}
		if hdr.Typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = e.Size
		}
		// PAX records would embed atime/ctime; strip them by using the
		// minimal deterministic header: FormatPAX only emits records when
		// fields overflow, so keep values small and stable.
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("archive: write header %s: %w", e.Name, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if open == nil {
				return fmt.Errorf("archive: no open func for file %s", e.Name)
			}
			rc, err := open(e)
			if err != nil {
				return fmt.Errorf("archive: open %s: %w", e.Name, err)
			}
			n, err := io.Copy(tw, rc)
			rc.Close()
			if err != nil {
				return fmt.Errorf("archive: write %s: %w", e.Name, err)
			}
			if n != e.Size {
				return fmt.Errorf("archive: %s changed size while archiving (%d != %d)", e.Name, n, e.Size)
			}
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("archive: close: %w", err)
	}
	return nil
}

// NormalizeMode converts fs.FileMode into deterministic tar permission
// bits. Only the permission bits are honoured; setuid/setgid/sticky and
// all type bits are dropped so host-specific modes cannot leak into
// digests.
func NormalizeMode(m fs.FileMode) int64 {
	return int64(m.Perm())
}
