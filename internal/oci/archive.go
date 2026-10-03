package oci

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/cuixudong74/container-image-builder-go/internal/digest"
)

// Blob is a content-addressed blob to include in the layout.
type Blob struct {
	Digest digest.Digest
	Data   []byte
}

// WriteLayout writes an OCI image layout directory to dir.
func WriteLayout(dir string, idx *Index, blobs []Blob) error {
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		return err
	}
	// oci-layout marker
	layout := Layout{ImageLayoutVersion: "1.0.0"}
	if err := writeJSON(filepath.Join(dir, "oci-layout"), layout); err != nil {
		return err
	}
	// blobs
	seen := map[string]bool{}
	for _, b := range blobs {
		hex := b.Digest.Hex()
		if seen[hex] {
			continue
		}
		seen[hex] = true
		p := filepath.Join(dir, "blobs", "sha256", hex)
		if err := os.WriteFile(p, b.Data, 0o644); err != nil {
			return err
		}
	}
	// index.json
	if err := writeJSON(filepath.Join(dir, "index.json"), idx); err != nil {
		return err
	}
	return nil
}

func writeJSON(path string, v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// WriteArchive tars the layout directory into w as an OCI archive.
// Entries are sorted and timestamped with epoch for reproducibility.
func WriteArchive(w io.Writer, layoutDir string, epoch time.Time) error {
	tw := tar.NewWriter(w)
	type fileEntry struct {
		rel  string
		path string
	}
	var files []fileEntry
	err := filepath.Walk(layoutDir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(layoutDir, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if fi.IsDir() {
			if rel == "." {
				return nil
			}
			rel += "/"
		}
		files = append(files, fileEntry{rel: rel, path: path})
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	for _, f := range files {
		fi, err := os.Stat(f.path)
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Name:       f.rel,
			Mode:       0o644,
			Uid:        0,
			Gid:        0,
			ModTime:    epoch,
			AccessTime: epoch,
			ChangeTime: epoch,
		}
		if fi.IsDir() {
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0o755
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = fi.Size()
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("tar header %q: %w", f.rel, err)
		}
		if !fi.IsDir() {
			in, err := os.Open(f.path)
			if err != nil {
				return err
			}
			if _, err := io.Copy(tw, in); err != nil {
				in.Close()
				return err
			}
			in.Close()
		}
	}
	return tw.Close()
}

// MarshalJSONDeterministic marshals v with sorted map keys (Go's default)
// and returns the bytes plus their digest.
func MarshalJSONDeterministic(v interface{}) ([]byte, digest.Digest, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, "", err
	}
	d := digest.FromBytes(data)
	return data, d, nil
}
