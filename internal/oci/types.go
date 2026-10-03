// Package oci implements OCI image layout types and deterministic serialization.
package oci

import (
	"time"

	"github.com/cuixudong74/container-image-builder-go/internal/digest"
)

// Media types used in OCI image manifests.
const (
	MediaTypeManifest = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeIndex    = "application/vnd.oci.image.index.v1+json"
	MediaTypeConfig   = "application/vnd.oci.image.config.v1+json"
	MediaTypeLayerTar = "application/vnd.oci.image.layer.v1.tar"
)

// Descriptor references a content-addressed blob.
type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      digest.Digest     `json:"digest"`
	Size        int64             `json:"size"`
	URLs        []string          `json:"urls,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    *Platform         `json:"platform,omitempty"`
}

// Platform is the OCI platform descriptor.
type Platform struct {
	OS           string   `json:"os"`
	Architecture string   `json:"architecture"`
	Variant      string   `json:"variant,omitempty"`
	OSVersion    string   `json:"os.version,omitempty"`
	OSFeatures   []string `json:"os.features,omitempty"`
}

// ImageConfig is the OCI image configuration blob (config.json).
type ImageConfig struct {
	Created      time.Time     `json:"created"`
	Author       string        `json:"author,omitempty"`
	Platform     Platform      `json:"platform"`
	Config       RuntimeConfig `json:"config"`
	RootFS       RootFS        `json:"rootfs"`
	History      []History     `json:"history,omitempty"`
	OS           string        `json:"os,omitempty"`
	Architecture string        `json:"architecture,omitempty"`
}

// RuntimeConfig holds the runtime configuration of an image.
type RuntimeConfig struct {
	User         string              `json:"user,omitempty"`
	ExposedPorts map[string]struct{} `json:"exposedPorts,omitempty"`
	Env          []string            `json:"env,omitempty"`
	Entrypoint   []string            `json:"entrypoint,omitempty"`
	Cmd          []string            `json:"cmd,omitempty"`
	Volumes      map[string]struct{} `json:"volumes,omitempty"`
	WorkingDir   string              `json:"workingDir,omitempty"`
	Labels       map[string]string   `json:"labels,omitempty"`
	StopSignal   string              `json:"stopSignal,omitempty"`
}

// RootFS describes the layer diff IDs.
type RootFS struct {
	Type    string          `json:"type"`
	DiffIDs []digest.Digest `json:"diff_ids"`
}

// History is an entry in the image history.
type History struct {
	Created    time.Time `json:"created"`
	CreatedBy  string    `json:"created_by,omitempty"`
	Comment    string    `json:"comment,omitempty"`
	EmptyLayer bool      `json:"empty_layer,omitempty"`
}

// Manifest is an OCI image manifest.
type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Config        Descriptor        `json:"config"`
	Layers        []Descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// Index is an OCI image index (manifest list).
type Index struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Manifests     []Descriptor      `json:"manifests"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// Layout is the oci-layout marker file.
type Layout struct {
	ImageLayoutVersion string `json:"imageLayoutVersion"`
}

// NewImageConfig returns an initialized config for a platform.
func NewImageConfig(p Platform, epoch time.Time) *ImageConfig {
	return &ImageConfig{
		Created:      epoch,
		Platform:     p,
		OS:           p.OS,
		Architecture: p.Architecture,
		Config: RuntimeConfig{
			Env:          []string{},
			ExposedPorts: map[string]struct{}{},
			Volumes:      map[string]struct{}{},
			Labels:       map[string]string{},
		},
		RootFS: RootFS{Type: "layers", DiffIDs: []digest.Digest{}},
		History: []History{},
	}
}
