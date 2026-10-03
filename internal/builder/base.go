package builder

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cuixudong74/container-image-builder-go/internal/digest"
	"github.com/cuixudong74/container-image-builder-go/internal/oci"
)

// BaseImage is a loaded base image (from a local OCI/docker archive tar).
type BaseImage struct {
	Name           string
	Platform       Platform
	Config         *oci.ImageConfig
	LayerBlobs     [][]byte // uncompressed layer tars in order
	ManifestDigest digest.Digest
}

type tarEntry struct {
	header *tar.Header
	data   []byte
}

// LoadBaseImage loads a base image for platform p from a local tar archive.
// Both OCI layout archives (index.json) and docker-save archives
// (manifest.json) are supported; no network access is performed.
func LoadBaseImage(name, tarPath string, p Platform) (*BaseImage, error) {
	data, err := os.ReadFile(tarPath)
	if err != nil {
		return nil, fmt.Errorf("load base image %q from %q: %w", name, tarPath, err)
	}
	entries, err := readTar(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("load base image %q: %w", name, err)
	}
	byName := map[string]*tarEntry{}
	for _, e := range entries {
		byName[e.header.Name] = e
	}

	if idx, ok := byName["index.json"]; ok {
		return loadOCIBase(name, p, idx.data, byName)
	}
	if mf, ok := byName["manifest.json"]; ok {
		return loadDockerBase(name, p, mf.data, byName)
	}
	return nil, fmt.Errorf("base image %q: archive contains neither index.json nor manifest.json", name)
}

func loadOCIBase(name string, p Platform, indexData []byte, byName map[string]*tarEntry) (*BaseImage, error) {
	var idx oci.Index
	if err := json.Unmarshal(indexData, &idx); err != nil {
		return nil, fmt.Errorf("base image %q: parse index.json: %w", name, err)
	}
	for _, md := range idx.Manifests {
		if md.Platform == nil || !platformMatches(*md.Platform, p) {
			continue
		}
		me, ok := byName["blobs/sha256/"+md.Digest.Hex()]
		if !ok {
			return nil, fmt.Errorf("base image %q: manifest blob %s missing", name, md.Digest)
		}
		var man oci.Manifest
		if err := json.Unmarshal(me.data, &man); err != nil {
			return nil, fmt.Errorf("base image %q: parse manifest: %w", name, err)
		}
		return baseFromManifest(name, p, md.Digest, &man, byName)
	}
	return nil, fmt.Errorf("base image %q: no manifest for platform %s", name, p)
}

func loadDockerBase(name string, p Platform, manifestData []byte, byName map[string]*tarEntry) (*BaseImage, error) {
	var manifests []struct {
		Config string   `json:"Config"`
		Layers []string `json:"Layers"`
	}
	if err := json.Unmarshal(manifestData, &manifests); err != nil {
		return nil, fmt.Errorf("base image %q: parse manifest.json: %w", name, err)
	}
	for _, m := range manifests {
		ce, ok := byName[m.Config]
		if !ok {
			continue
		}
		var cfg oci.ImageConfig
		if err := json.Unmarshal(ce.data, &cfg); err != nil {
			continue
		}
		if cfg.OS != p.OS || cfg.Architecture != p.Arch || (cfg.Platform.Variant != "" && cfg.Platform.Variant != p.Variant) {
			continue
		}
		bi := &BaseImage{Name: name, Platform: p, Config: &cfg, ManifestDigest: digest.FromBytes(ce.data)}
		for _, l := range m.Layers {
			le, ok := byName[l]
			if !ok {
				return nil, fmt.Errorf("base image %q: layer %s missing", name, l)
			}
			bi.LayerBlobs = append(bi.LayerBlobs, le.data)
		}
		return bi, nil
	}
	return nil, fmt.Errorf("base image %q: no image for platform %s", name, p)
}

func baseFromManifest(name string, p Platform, md digest.Digest, man *oci.Manifest, byName map[string]*tarEntry) (*BaseImage, error) {
	ce, ok := byName["blobs/sha256/"+man.Config.Digest.Hex()]
	if !ok {
		return nil, fmt.Errorf("base image %q: config blob %s missing", name, man.Config.Digest)
	}
	var cfg oci.ImageConfig
	if err := json.Unmarshal(ce.data, &cfg); err != nil {
		return nil, fmt.Errorf("base image %q: parse config: %w", name, err)
	}
	bi := &BaseImage{Name: name, Platform: p, Config: &cfg, ManifestDigest: md}
	for _, ld := range man.Layers {
		le, ok := byName["blobs/sha256/"+ld.Digest.Hex()]
		if !ok {
			return nil, fmt.Errorf("base image %q: layer blob %s missing", name, ld.Digest)
		}
		bi.LayerBlobs = append(bi.LayerBlobs, le.data)
	}
	return bi, nil
}

func platformMatches(got oci.Platform, want Platform) bool {
	if got.OS != want.OS || got.Architecture != want.Arch {
		return false
	}
	if want.Variant != "" && got.Variant != want.Variant {
		return false
	}
	return true
}

// ExtractLayers extracts the base image layers into rootfs.
func (b *BaseImage) ExtractLayers(rootfs string) error {
	for i, blob := range b.LayerBlobs {
		if err := extractTar(blob, rootfs); err != nil {
			return fmt.Errorf("extract base layer %d: %w", i, err)
		}
	}
	return nil
}

func readTar(r io.Reader) ([]*tarEntry, error) {
	tr := tar.NewReader(r)
	var out []*tarEntry
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out = append(out, &tarEntry{header: hdr, data: data})
	}
	return out, nil
}

// extractTar extracts a tar stream into rootfs with path-traversal protection.
func extractTar(data []byte, rootfs string) error {
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(rootfs, hdr.Name)
		if err != nil {
			return err
		}
		// OCI whiteout entry: .wh.<name> deletes the corresponding path.
		if base := filepath.Base(hdr.Name); strings.HasPrefix(base, ".wh.") {
			whName := strings.TrimPrefix(base, ".wh.")
			whTarget := filepath.Join(filepath.Dir(target), whName)
			if err := os.RemoveAll(whTarget); err != nil {
				return err
			}
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
			if err := os.Chmod(target, os.FileMode(hdr.Mode)&0o7777); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			linkTarget, err := safeJoin(rootfs, hdr.Linkname)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			os.Remove(target)
			if err := os.Link(linkTarget, target); err != nil {
				return err
			}
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			// device nodes are not supported in rootless builds; skip
			continue
		default:
			continue
		}
	}
	return nil
}

// safeJoin joins rootfs and name, rejecting traversal and absolute paths.
func safeJoin(rootfs, name string) (string, error) {
	clean := filepath.Clean(name)
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("unsafe path in archive: %q", name)
	}
	target := filepath.Join(rootfs, clean)
	if !strings.HasPrefix(filepath.Clean(target)+string(filepath.Separator), filepath.Clean(rootfs)+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes rootfs: %q", name)
	}
	return target, nil
}
