package engine

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cuixudong74/container-image-builder-go/pkg/image"
)

// assembleManifest builds the image config and manifest for one platform
// from the final stage state, and returns the manifest descriptor.
func (e *Engine) assembleManifest(st *stageState, plat image.Platform) (image.Descriptor, error) {
	cfg := image.Config{
		Created:      e.epoch,
		Architecture: plat.Architecture,
		OS:           plat.OS,
		Config: image.ConfigSpec{
			User:         st.cfg.user,
			Env:          st.cfg.env,
			Entrypoint:   st.cfg.entrypoint,
			Cmd:          st.cfg.cmd,
			Labels:       st.cfg.labels,
			ExposedPorts: st.cfg.ports,
		},
		RootFS:  image.RootFS{Type: "layers"},
		History: st.history,
	}
	if st.cfg.workdir != "" {
		cfg.Config.WorkingDir = "/" + st.cfg.workdir
	}
	manifest := image.Manifest{SchemaVersion: 2, MediaType: image.MediaTypeImageManifest}
	for _, l := range st.layers {
		cfg.RootFS.DiffIDs = append(cfg.RootFS.DiffIDs, l.Digest)
		manifest.Layers = append(manifest.Layers, image.Descriptor{
			MediaType: image.MediaTypeLayerTar,
			Digest:    l.Digest,
			Size:      l.Size,
		})
	}
	cfgJSON, err := image.MarshalCanonical(cfg)
	if err != nil {
		return image.Descriptor{}, fmt.Errorf("marshal config: %w", err)
	}
	cfgDigest := image.Digest(cfgJSON)
	if err := e.storeBlob(cfgDigest, cfgJSON); err != nil {
		return image.Descriptor{}, err
	}
	manifest.Config = image.Descriptor{
		MediaType: image.MediaTypeImageConfig,
		Digest:    cfgDigest,
		Size:      int64(len(cfgJSON)),
	}
	mJSON, err := image.MarshalCanonical(manifest)
	if err != nil {
		return image.Descriptor{}, fmt.Errorf("marshal manifest: %w", err)
	}
	mDigest := image.Digest(mJSON)
	if err := e.storeBlob(mDigest, mJSON); err != nil {
		return image.Descriptor{}, err
	}
	// Record the manifest blob for the output writer.
	e.addOutputBlob(mDigest)
	e.addOutputBlob(cfgDigest)
	for _, l := range st.layers {
		e.addOutputBlob(l.Digest)
	}
	return image.Descriptor{
		MediaType: image.MediaTypeImageManifest,
		Digest:    mDigest,
		Size:      int64(len(mJSON)),
		Platform:  &plat,
	}, nil
}

// storeBlob writes a small JSON blob into the build-scratch blob store.
func (e *Engine) storeBlob(digest string, data []byte) error {
	p := filepath.Join(e.tmpDir, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// writeOutput assembles the final OCI layout in a temp directory and
// atomically swaps it into place, so a cancelled or failed build never
// leaves a half-written output.
func (e *Engine) writeOutput(idx image.Index) (string, error) {
	out := filepath.Join(e.tmpDir, "layout")
	blobs := filepath.Join(out, "blobs", "sha256")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(out, "oci-layout"),
		[]byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		return "", err
	}

	// Copy referenced blobs: JSON blobs from scratch, layer blobs from the
	// shared cache.
	for _, d := range e.outputBlobs() {
		hexPart := strings.TrimPrefix(d, "sha256:")
		dst := filepath.Join(blobs, hexPart)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		src := filepath.Join(e.tmpDir, "blobs", "sha256", hexPart)
		if _, err := os.Stat(src); err != nil {
			e.blobMu.Lock()
			alt, hasAlt := e.blobSrc[d]
			e.blobMu.Unlock()
			if hasAlt {
				src = alt
			} else if cachePath, ok := e.cache.BlobPath(d); ok {
				src = cachePath
			} else {
				return "", fmt.Errorf("layer blob %s missing from cache", d)
			}
		}
		if err := copyFileAtomic(dst, src); err != nil {
			return "", fmt.Errorf("write blob %s: %w", d, err)
		}
	}

	idxJSON, err := image.MarshalCanonical(idx)
	if err != nil {
		return "", fmt.Errorf("marshal index: %w", err)
	}
	if err := os.WriteFile(filepath.Join(out, "index.json"), idxJSON, 0o644); err != nil {
		return "", err
	}

	// Atomic swap: move any previous output aside, move the new one in,
	// then drop the old. A crash anywhere leaves a valid old or new output.
	final := e.opts.OutputDir
	backup := final + ".old-" + itoa(os.Getpid())
	_ = os.RemoveAll(backup)
	if _, err := os.Stat(final); err == nil {
		if err := os.Rename(final, backup); err != nil {
			return "", fmt.Errorf("replace output: %w", err)
		}
	}
	if err := os.Rename(out, final); err != nil {
		return "", fmt.Errorf("commit output: %w", err)
	}
	_ = os.RemoveAll(backup)
	return image.Digest(idxJSON), nil
}

func copyFileAtomic(dst, src string) error {
	tmp := dst + ".tmp"
	if err := copyFile(tmp, src); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func copyFile(dst, src string) (retErr error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			out.Close()
			os.Remove(dst)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	return out.Close()
}
