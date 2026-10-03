// Package image defines the OCI image configuration, manifest and index
// types plus canonical (deterministic) JSON serialisation and digests.
package image

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Media types used by the produced OCI layout.
const (
	MediaTypeImageManifest = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeImageIndex    = "application/vnd.oci.image.index.v1+json"
	MediaTypeImageConfig   = "application/vnd.oci.image.config.v1+json"
	MediaTypeLayerTar      = "application/vnd.oci.image.layer.v1.tar"
)

// Descriptor references a blob by digest.
type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Platform    *Platform         `json:"platform,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Platform identifies an os/arch pair.
type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}

// Config is the OCI image configuration document.
type Config struct {
	Created      time.Time  `json:"created"`
	Architecture string     `json:"architecture"`
	OS           string     `json:"os"`
	Config       ConfigSpec `json:"config"`
	RootFS       RootFS     `json:"rootfs"`
	History      []History  `json:"history"`
}

// ConfigSpec is the runtime config section.
type ConfigSpec struct {
	User         string              `json:"User,omitempty"`
	Env          []string            `json:"Env,omitempty"`
	Entrypoint   []string            `json:"Entrypoint,omitempty"`
	Cmd          []string            `json:"Cmd,omitempty"`
	WorkingDir   string              `json:"WorkingDir,omitempty"`
	Labels       map[string]string   `json:"Labels,omitempty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
}

// RootFS lists layer diff IDs.
type RootFS struct {
	Type    string   `json:"type"`
	DiffIDs []string `json:"diff_ids"`
}

// History is one history entry.
type History struct {
	Created    time.Time `json:"created"`
	CreatedBy  string    `json:"created_by"`
	EmptyLayer bool      `json:"empty_layer,omitempty"`
}

// Manifest is the OCI image manifest.
type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
}

// Index is the OCI image index (manifest list).
type Index struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Manifests     []Descriptor `json:"manifests"`
}

// MarshalCanonical serialises v to deterministic JSON: struct field order
// is fixed by the type definitions, map keys are sorted by encoding/json,
// HTML escaping is disabled and no insignificant whitespace is emitted.
// Identical values always produce identical bytes.
func MarshalCanonical(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encoder.Encode appends a newline; strip it for stable digests.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Digest returns "sha256:<hex>" of b.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
