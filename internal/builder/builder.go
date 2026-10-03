package builder

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/cuixudong74/container-image-builder-go/internal/buildctx"
	"github.com/cuixudong74/container-image-builder-go/internal/cache"
	"github.com/cuixudong74/container-image-builder-go/internal/description"
	"github.com/cuixudong74/container-image-builder-go/internal/digest"
	"github.com/cuixudong74/container-image-builder-go/internal/oci"
)

// BuildRequest describes a build invocation.
type BuildRequest struct {
	DescriptionFile string
	ContextDir      string
	Output          string // output OCI archive tar path
	Tags            []string
	Platforms       []Platform
	Bases           map[string]string
	CacheDir        string
	Epoch           int64 // unix seconds used for all timestamps
	MaxLayerSize    int64
	MaxFileSize     int64
	Workers         int
}

// BuildResult summarizes a successful build.
type BuildResult struct {
	Output          string
	IndexDigest     digest.Digest
	ManifestDigests map[string]digest.Digest // platform -> manifest digest
	ConfigDigests   map[string]digest.Digest
}

// Build runs a build and writes the OCI archive.
func Build(ctx context.Context, req *BuildRequest) (*BuildResult, error) {
	if req.DescriptionFile == "" {
		req.DescriptionFile = "Dockerfile"
	}
	if req.ContextDir == "" {
		req.ContextDir = "."
	}
	if req.Output == "" {
		req.Output = "image.tar"
	}
	if len(req.Platforms) == 0 {
		req.Platforms = []Platform{CurrentPlatform()}
	}
	if req.Workers <= 0 {
		req.Workers = 4
	}
	if req.Bases == nil {
		req.Bases = map[string]string{}
	}
	epoch := time.Unix(req.Epoch, 0).UTC()
	opts := BuildOptions{
		Epoch:        epoch,
		MaxLayerSize: req.MaxLayerSize,
		MaxFileSize:  req.MaxFileSize,
		Workers:      req.Workers,
	}

	// 1. Parse the build description.
	desc, err := description.ParseFile(req.DescriptionFile)
	if err != nil {
		return nil, err
	}

	// 2. Open the build context.
	ctx2, err := buildctx.Open(req.ContextDir, buildctx.Options{MaxFileSize: req.MaxFileSize})
	if err != nil {
		return nil, err
	}

	// 3. Open the layer cache.
	c, err := cache.Open(req.CacheDir)
	if err != nil {
		return nil, err
	}

	// 4. Build all stages for all platforms.
	graph := NewGraph(desc, req.Platforms, ctx2, c, req.Bases, opts)
	results, err := graph.Build(ctx)
	if err != nil {
		return nil, err
	}

	// 5. Assemble the OCI layout.
	layoutDir, err := os.MkdirTemp(c.TmpDir(), "layout-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(layoutDir)

	var blobs []oci.Blob
	index := &oci.Index{
		SchemaVersion: 2,
		MediaType:     oci.MediaTypeIndex,
		Manifests:     []oci.Descriptor{},
	}
	if len(req.Tags) > 0 {
		index.Annotations = map[string]string{"io.containerd.image.name": req.Tags[0]}
	}

	finalStage := len(desc.Stages) - 1
	platStrs := make([]string, 0, len(req.Platforms))
	platByStr := map[string]Platform{}
	for _, p := range req.Platforms {
		platStrs = append(platStrs, p.String())
		platByStr[p.String()] = p
	}
	sort.Strings(platStrs)

	res := &BuildResult{
		ManifestDigests: map[string]digest.Digest{},
		ConfigDigests:   map[string]digest.Digest{},
	}

	for _, ps := range platStrs {
		p := platByStr[ps]
		sr, ok := results[stageKey(finalStage, p)]
		if !ok {
			return nil, fmt.Errorf("internal: missing build result for platform %s", ps)
		}
		// config blob
		configData, _, err := oci.MarshalJSONDeterministic(sr.Config)
		if err != nil {
			return nil, err
		}
		blobs = append(blobs, oci.Blob{Digest: sr.ConfigDigest, Data: configData})
		// manifest blob
		manData, _, err := oci.MarshalJSONDeterministic(sr.Manifest)
		if err != nil {
			return nil, err
		}
		blobs = append(blobs, oci.Blob{Digest: sr.ManifestDigest, Data: manData})
		// layer blobs
		for _, d := range sr.Manifest.Layers {
			data, err := os.ReadFile(c.BlobPath(d.Digest))
			if err != nil {
				return nil, fmt.Errorf("read cached layer %s: %w", d.Digest, err)
			}
			blobs = append(blobs, oci.Blob{Digest: d.Digest, Data: data})
		}
		index.Manifests = append(index.Manifests, oci.Descriptor{
			MediaType: oci.MediaTypeManifest,
			Digest:    sr.ManifestDigest,
			Size:      int64(len(manData)),
			Platform:  &oci.Platform{OS: p.OS, Architecture: p.Arch, Variant: p.Variant},
		})
		res.ManifestDigests[ps] = sr.ManifestDigest
		res.ConfigDigests[ps] = sr.ConfigDigest
	}

	if err := oci.WriteLayout(layoutDir, index, blobs); err != nil {
		return nil, err
	}

	// 6. Write the archive atomically.
	indexData, indexDigest, err := oci.MarshalJSONDeterministic(index)
	if err != nil {
		return nil, err
	}
	_ = indexData
	res.IndexDigest = indexDigest

	if err := writeArchiveAtomic(req.Output, layoutDir, epoch); err != nil {
		return nil, err
	}
	res.Output = req.Output
	return res, nil
}

// writeArchiveAtomic writes the OCI archive to output via a temp file + rename.
func writeArchiveAtomic(output, layoutDir string, epoch time.Time) error {
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(output), ".image-*.tar.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := oci.WriteArchive(tmp, layoutDir, epoch); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, output)
}
