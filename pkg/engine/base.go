package engine

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cuixudong74/container-image-builder-go/pkg/image"
)

// baseImage is a loaded FROM oci: base.
type baseImage struct {
	fs             *vfs
	layers         []layerRef
	cfg            configState
	manifestDigest string
}

// loadBase reads an OCI image layout from dir, selects the manifest for
// plat (erroring clearly when the platform is absent) and extracts its
// uncompressed tar layers into the build scratch dir.
func (e *Engine) loadBase(dir string, plat image.Platform) (*baseImage, error) {
	idxData, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		return nil, fmt.Errorf("read base image index: %w", err)
	}
	var idx image.Index
	if err := json.Unmarshal(idxData, &idx); err != nil {
		return nil, fmt.Errorf("parse base image index: %w", err)
	}
	var mdesc *image.Descriptor
	for i := range idx.Manifests {
		m := &idx.Manifests[i]
		if m.Platform != nil && m.Platform.OS == plat.OS && m.Platform.Architecture == plat.Architecture {
			mdesc = m
			break
		}
	}
	if mdesc == nil {
		var have []string
		for _, m := range idx.Manifests {
			if m.Platform != nil {
				have = append(have, m.Platform.OS+"/"+m.Platform.Architecture)
			}
		}
		return nil, fmt.Errorf("base image %s has no manifest for platform %s/%s (available: %s)",
			dir, plat.OS, plat.Architecture, strings.Join(have, ", "))
	}

	mData, err := readBlob(dir, mdesc.Digest)
	if err != nil {
		return nil, fmt.Errorf("read base manifest: %w", err)
	}
	var manifest image.Manifest
	if err := json.Unmarshal(mData, &manifest); err != nil {
		return nil, fmt.Errorf("parse base manifest: %w", err)
	}
	cfgData, err := readBlob(dir, manifest.Config.Digest)
	if err != nil {
		return nil, fmt.Errorf("read base config: %w", err)
	}
	var cfg image.Config
	if err := json.Unmarshal(cfgData, &cfg); err != nil {
		return nil, fmt.Errorf("parse base config: %w", err)
	}

	base := &baseImage{
		fs:             newVFS(),
		cfg:            configFromImage(cfg.Config),
		manifestDigest: mdesc.Digest,
	}
	extractDir, err := os.MkdirTemp(e.tmpDir, "base-*")
	if err != nil {
		return nil, err
	}
	for _, ld := range manifest.Layers {
		if ld.MediaType != image.MediaTypeLayerTar {
			return nil, fmt.Errorf("base layer %s uses unsupported media type %s (only uncompressed tar is supported)", ld.Digest, ld.MediaType)
		}
		p := filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(ld.Digest, "sha256:"))
		if err := e.extractLayer(p, extractDir, base.fs); err != nil {
			return nil, fmt.Errorf("extract base layer %s: %w", ld.Digest, err)
		}
		base.layers = append(base.layers, layerRef{Digest: ld.Digest, Size: ld.Size})
		// Base layers are not in this build's cache; remember where the
		// output writer can find them.
		e.blobMu.Lock()
		e.blobSrc[ld.Digest] = p
		e.blobMu.Unlock()
		e.addOutputBlob(ld.Digest)
	}
	return base, nil
}

func readBlob(dir, digest string) ([]byte, error) {
	return os.ReadFile(filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:")))
}

// extractLayer unpacks one tar layer into dir and registers its files in fs.
func (e *Engine) extractLayer(tarPath, dir string, fs *vfs) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		name := cleanPath(hdr.Name)
		if name == "" {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			fs.set(name, &vfsNode{Mode: hdr.Mode & 0o777, IsDir: true})
		case tar.TypeSymlink:
			fs.set(name, &vfsNode{Mode: 0o777, Link: hdr.Linkname})
		case tar.TypeReg:
			dst := filepath.Join(dir, "f-"+hex.EncodeToString(sha256.New().Sum([]byte(name))[:8])+"-"+itoa(len(fs.nodes)))
			if err := writeExtracted(dst, tr, hdr.Size); err != nil {
				return err
			}
			dg, err := digestHostFile(dst)
			if err != nil {
				return err
			}
			fs.set(name, &vfsNode{Mode: hdr.Mode & 0o777, Size: hdr.Size, Digest: dg, Src: dst})
		default:
			return fmt.Errorf("unsupported tar entry type %d for %s", hdr.Typeflag, name)
		}
		fs.ensureParents(name)
	}
}

func writeExtracted(dst string, r io.Reader, size int64) (retErr error) {
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
	n, err := io.Copy(out, r)
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("short extract: %d != %d", n, size)
	}
	return out.Close()
}

func digestHostFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// configFromImage converts a base image's runtime config.
func configFromImage(c image.ConfigSpec) configState {
	cs := newConfigState()
	cs.user = c.User
	cs.entrypoint = append([]string(nil), c.Entrypoint...)
	cs.cmd = append([]string(nil), c.Cmd...)
	cs.workdir = cleanPath(c.WorkingDir)
	for _, kv := range c.Env {
		if i := strings.Index(kv, "="); i > 0 {
			cs.setEnv(kv[:i], kv[i+1:])
		}
	}
	for k, v := range c.Labels {
		cs.labels[k] = v
	}
	for p := range c.ExposedPorts {
		cs.ports[p] = struct{}{}
	}
	return cs
}
