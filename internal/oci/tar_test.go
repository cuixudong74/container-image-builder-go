package oci

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteLayerTarDeterministic(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("world"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := []LayerFile{
		{RelPath: "a.txt", AbsPath: filepath.Join(dir, "a.txt"), Type: LayerRegular},
		{RelPath: "b.txt", AbsPath: filepath.Join(dir, "b.txt"), Type: LayerRegular},
		{RelPath: "sub", Type: LayerDir},
	}
	epoch := time.Unix(0, 0)

	var buf1 bytes.Buffer
	d1, n1, err := WriteLayerTar(&buf1, epoch, files, nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf2 bytes.Buffer
	d2, n2, err := WriteLayerTar(&buf2, epoch, files, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Error("digest differs between identical layer writes")
	}
	if n1 != n2 {
		t.Error("size differs between identical layer writes")
	}
	if !bytes.Equal(buf1.Bytes(), buf2.Bytes()) {
		t.Error("tar bytes differ between identical writes")
	}
}

func TestWriteLayerTarCanonicalModes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o777); err != nil {
		t.Fatal(err)
	}
	files := []LayerFile{
		{RelPath: "a.txt", AbsPath: filepath.Join(dir, "a.txt"), Type: LayerRegular},
	}
	var buf bytes.Buffer
	WriteLayerTar(&buf, time.Unix(0, 0), files, nil)
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == "a.txt" && hdr.Mode != 0o644 {
			t.Errorf("mode = %o, want 0644", hdr.Mode)
		}
	}
}

func TestWriteLayerTarWhiteout(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := []LayerFile{
		{RelPath: "a.txt", AbsPath: filepath.Join(dir, "a.txt"), Type: LayerRegular},
	}
	deletions := []string{"removed.txt"}
	var buf bytes.Buffer
	WriteLayerTar(&buf, time.Unix(0, 0), files, deletions)
	tr := tar.NewReader(&buf)
	found := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == ".wh.removed.txt" {
			found = true
		}
	}
	if !found {
		t.Error("whiteout entry .wh.removed.txt not found")
	}
}
