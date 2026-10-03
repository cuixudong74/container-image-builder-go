package image

import (
	"testing"
	"time"
)

func TestMarshalCanonicalDeterministic(t *testing.T) {
	cfg := Config{
		Created:      time.Unix(0, 0).UTC(),
		Architecture: "amd64",
		OS:           "linux",
		Config: ConfigSpec{
			Env:    []string{"A=1", "B=2"},
			Labels: map[string]string{"z": "1", "a": "2"},
		},
		RootFS: RootFS{Type: "layers", DiffIDs: []string{"sha256:abc"}},
	}
	b1, err := MarshalCanonical(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := MarshalCanonical(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(b1) != string(b2) {
		t.Fatal("canonical marshal not deterministic")
	}
	// Map keys must be sorted, no trailing newline, no HTML escaping.
	s := string(b1)
	if s[len(s)-1] == '\n' {
		t.Fatal("trailing newline present")
	}
	if want := `"Labels":{"a":"2","z":"1"}`; !contains(s, want) {
		t.Fatalf("map keys not sorted, want %s in %s", want, s)
	}
}

func TestDigest(t *testing.T) {
	// sha256("hello") is a well-known constant.
	got := Digest([]byte("hello"))
	want := "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got != want {
		t.Fatalf("Digest: got %s want %s", got, want)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
