package archive

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

func testEntries() []Entry {
	return []Entry{
		{Name: "app", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "app/data.txt", Mode: 0o644, Size: 5},
		{Name: "app/link", Typeflag: tar.TypeSymlink, Mode: 0o777, Linkname: "data.txt"},
	}
}

func openFunc(content string) OpenFunc {
	return func(e Entry) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(content)), nil
	}
}

func TestDeterministicBytes(t *testing.T) {
	epoch := time.Unix(0, 0).UTC()
	var b1, b2 bytes.Buffer
	if err := WriteTar(&b1, epoch, testEntries(), openFunc("hello")); err != nil {
		t.Fatal(err)
	}
	if err := WriteTar(&b2, epoch, testEntries(), openFunc("hello")); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1.Bytes(), b2.Bytes()) {
		t.Fatal("identical entries produced different bytes")
	}
	// Different epoch changes bytes (epoch is part of the config contract).
	var b3 bytes.Buffer
	if err := WriteTar(&b3, time.Unix(100, 0).UTC(), testEntries(), openFunc("hello")); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(b1.Bytes(), b3.Bytes()) {
		t.Fatal("epoch should affect output bytes")
	}
}

func TestHeaderNormalization(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteTar(&buf, time.Unix(0, 0).UTC(), testEntries(), openFunc("hello")); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&buf)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
		if hdr.Uid != 0 || hdr.Gid != 0 || hdr.Uname != "" || hdr.Gname != "" {
			t.Fatalf("identity not zeroed: %+v", hdr)
		}
		if !hdr.ModTime.Equal(time.Unix(0, 0).UTC()) {
			t.Fatalf("mtime not pinned: %v", hdr.ModTime)
		}
		if hdr.Name == "app/data.txt" {
			data, _ := io.ReadAll(tr)
			if string(data) != "hello" {
				t.Fatalf("content: %q", data)
			}
		}
	}
	want := []string{"app", "app/data.txt", "app/link"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("names: %v", names)
	}
}

func TestSizeMismatchFails(t *testing.T) {
	var buf bytes.Buffer
	entries := []Entry{{Name: "f", Mode: 0o644, Size: 10}}
	err := WriteTar(&buf, time.Unix(0, 0).UTC(), entries, openFunc("short"))
	if err == nil || !strings.Contains(err.Error(), "changed size") {
		t.Fatalf("want size mismatch error, got %v", err)
	}
}

func TestNormalizeMode(t *testing.T) {
	if got := NormalizeMode(0o100755); got != 0o755 {
		t.Fatalf("NormalizeMode: %o", got)
	}
	if got := NormalizeMode(0o104700); got != 0o700 {
		t.Fatalf("setuid bits must be dropped: %o", got)
	}
}
