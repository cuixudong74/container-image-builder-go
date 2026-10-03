package builder

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cuixudong74/container-image-builder-go/internal/digest"
	"github.com/cuixudong74/container-image-builder-go/internal/oci"
)

// buildEnv creates a temp context and description for testing.
type buildEnv struct {
	t       *testing.T
	ctxDir  string
	cacheDir string
	outDir  string
}

func newBuildEnv(t *testing.T) *buildEnv {
	t.Helper()
	return &buildEnv{
		t:        t,
		ctxDir:   t.TempDir(),
		cacheDir: t.TempDir(),
		outDir:   t.TempDir(),
	}
}

func (e *buildEnv) writeFile(rel, content string) {
	e.t.Helper()
	p := filepath.Join(e.ctxDir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *buildEnv) writeDesc(content string) string {
	e.t.Helper()
	p := filepath.Join(e.outDir, "Dockerfile")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *buildEnv) build(descPath string, platforms ...string) (*BuildResult, string) {
	e.t.Helper()
	out := filepath.Join(e.outDir, "image.tar")
	plats := []Platform{CurrentPlatform()}
	if len(platforms) > 0 {
		var err error
		plats, err = ParsePlatforms(strings.Join(platforms, ","))
		if err != nil {
			e.t.Fatal(err)
		}
	}
	req := &BuildRequest{
		DescriptionFile: descPath,
		ContextDir:      e.ctxDir,
		Output:          out,
		Platforms:       plats,
		CacheDir:        e.cacheDir,
		Epoch:           0,
		Workers:         2,
	}
	res, err := Build(context.Background(), req)
	if err != nil {
		e.t.Fatalf("build: %v", err)
	}
	return res, out
}

// readArchive extracts the OCI archive and returns index + blobs by digest.
func readArchive(t *testing.T, tarPath string) (*oci.Index, map[string][]byte) {
	t.Helper()
	data, err := os.ReadFile(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	blobs := map[string][]byte{}
	var index *oci.Index
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == "index.json" {
			index = &oci.Index{}
			if err := json.Unmarshal(content, index); err != nil {
				t.Fatal(err)
			}
		} else if strings.HasPrefix(hdr.Name, "blobs/sha256/") {
			d := strings.TrimPrefix(hdr.Name, "blobs/sha256/")
			blobs[d] = content
		}
	}
	if index == nil {
		t.Fatal("no index.json in archive")
	}
	return index, blobs
}

func TestBuildScratchCopy(t *testing.T) {
	env := newBuildEnv(t)
	env.writeFile("hello.txt", "hello world")
	desc := env.writeDesc("FROM scratch\nCOPY hello.txt /app/hello.txt\n")
	_, out := env.build(desc)
	index, blobs := readArchive(t, out)
	if len(index.Manifests) != 1 {
		t.Fatalf("expected 1 manifest, got %d", len(index.Manifests))
	}
	mf := index.Manifests[0]
	var man oci.Manifest
	if err := json.Unmarshal(blobs[mf.Digest.Hex()], &man); err != nil {
		t.Fatal(err)
	}
	if len(man.Layers) != 1 {
		t.Fatalf("expected 1 layer, got %d", len(man.Layers))
	}
	// Verify layer content.
	layerData := blobs[man.Layers[0].Digest.Hex()]
	tr := tar.NewReader(bytes.NewReader(layerData))
	found := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if hdr.Name == "app/hello.txt" {
			found = true
			if hdr.Mode != 0o644 {
				t.Errorf("mode = %o, want 0644", hdr.Mode)
			}
			if hdr.Uid != 0 || hdr.Gid != 0 {
				t.Errorf("uid/gid = %d/%d, want 0/0", hdr.Uid, hdr.Gid)
			}
			if !hdr.ModTime.Equal(time.Unix(0, 0)) {
				t.Errorf("modtime = %v, want epoch", hdr.ModTime)
			}
		}
	}
	if !found {
		t.Error("app/hello.txt not found in layer")
	}
}

func TestBuildReproducible(t *testing.T) {
	env := newBuildEnv(t)
	env.writeFile("a.txt", "aaa")
	env.writeFile("b.txt", "bbb")
	desc := env.writeDesc("FROM scratch\nCOPY a.txt /a\nCOPY b.txt /b\n")

	_, out1 := env.build(desc)
	data1, err := os.ReadFile(out1)
	if err != nil {
		t.Fatal(err)
	}
	// Second build with fresh cache but same content.
	env.cacheDir = t.TempDir()
	_, out2 := env.build(desc)
	data2, err := os.ReadFile(out2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data1, data2) {
		t.Error("output differs between identical builds")
	}
}

func TestBuildCacheHitAndMiss(t *testing.T) {
	env := newBuildEnv(t)
	env.writeFile("a.txt", "aaa")
	desc := env.writeDesc("FROM scratch\nCOPY a.txt /a\n")

	// First build: cache miss.
	env.build(desc)
	entries1, _ := os.ReadDir(env.cacheDir + "/entries/sha256")
	count1 := len(entries1)

	// Second build: cache hit, no new entries.
	env.build(desc)
	entries2, _ := os.ReadDir(env.cacheDir + "/entries/sha256")
	count2 := len(entries2)
	if count2 != count1 {
		t.Errorf("cache entries changed on hit: %d -> %d", count1, count2)
	}

	// Change content: cache miss, new entry.
	env.writeFile("a.txt", "changed")
	env.build(desc)
	entries3, _ := os.ReadDir(env.cacheDir + "/entries/sha256")
	count3 := len(entries3)
	if count3 <= count2 {
		t.Errorf("cache entries did not grow on content change: %d -> %d", count2, count3)
	}
}

func TestBuildMultiStage(t *testing.T) {
	env := newBuildEnv(t)
	env.writeFile("src.txt", "source")
	desc := env.writeDesc(`FROM scratch AS builder
COPY src.txt /src.txt
FROM scratch AS final
COPY --from=builder /src.txt /dst.txt
`)
	_, out := env.build(desc)
	index, blobs := readArchive(t, out)
	mf := index.Manifests[0]
	var man oci.Manifest
	json.Unmarshal(blobs[mf.Digest.Hex()], &man)
	// final stage starts from scratch and copies from builder: 1 layer.
	if len(man.Layers) != 1 {
		t.Fatalf("expected 1 layer, got %d", len(man.Layers))
	}
	// Verify dst.txt exists in the final layer.
	layerData := blobs[man.Layers[0].Digest.Hex()]
	tr := tar.NewReader(bytes.NewReader(layerData))
	found := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if hdr.Name == "dst.txt" {
			found = true
		}
	}
	if !found {
		t.Error("dst.txt not found in final layer")
	}
}

func TestBuildMultiArch(t *testing.T) {
	env := newBuildEnv(t)
	env.writeFile("a.txt", "aaa")
	desc := env.writeDesc("FROM scratch\nCOPY a.txt /a\n")
	_, out := env.build(desc, "linux/amd64", "linux/arm64")
	index, _ := readArchive(t, out)
	if len(index.Manifests) != 2 {
		t.Fatalf("expected 2 manifests, got %d", len(index.Manifests))
	}
	plats := []string{}
	for _, m := range index.Manifests {
		plats = append(plats, m.Platform.OS+"/"+m.Platform.Architecture)
	}
	sort.Strings(plats)
	if plats[0] != "linux/amd64" || plats[1] != "linux/arm64" {
		t.Errorf("platforms = %v", plats)
	}
}

func TestBuildConcurrent(t *testing.T) {
	env := newBuildEnv(t)
	env.writeFile("a.txt", "aaa")
	desc := env.writeDesc("FROM scratch\nCOPY a.txt /a\n")

	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out := filepath.Join(env.outDir, "image"+string(rune('0'+i))+".tar")
			req := &BuildRequest{
				DescriptionFile: desc,
				ContextDir:      env.ctxDir,
				Output:          out,
				Platforms:       []Platform{CurrentPlatform()},
				CacheDir:        env.cacheDir,
				Epoch:           0,
				Workers:         2,
			}
			if _, err := Build(context.Background(), req); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent build error: %v", err)
	}
	// All outputs should be identical.
	var first []byte
	for i := 0; i < 4; i++ {
		data, err := os.ReadFile(filepath.Join(env.outDir, "image"+string(rune('0'+i))+".tar"))
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = data
		} else if !bytes.Equal(first, data) {
			t.Error("concurrent builds produced different outputs")
		}
	}
}

func TestBuildInterruptRecovery(t *testing.T) {
	env := newBuildEnv(t)
	// Create a moderately large context.
	for i := 0; i < 200; i++ {
		env.writeFile(filepath.Join("data", "f"+string(rune('a'+i%26))+string(rune('0'+i/26))+".bin"), "content")
	}
	desc := env.writeDesc("FROM scratch\nCOPY . /data/\n")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	req := &BuildRequest{
		DescriptionFile: desc,
		ContextDir:      env.ctxDir,
		Output:          filepath.Join(env.outDir, "interrupted.tar"),
		Platforms:       []Platform{CurrentPlatform()},
		CacheDir:        env.cacheDir,
		Epoch:           0,
		Workers:         1,
	}
	_, err := Build(ctx, req)
	if err == nil {
		// Build may have completed before cancellation; that's fine.
		t.Skip("build completed before cancellation")
	}
	// No temp files should remain.
	tmpEntries, _ := os.ReadDir(filepath.Join(env.cacheDir, "tmp"))
	for _, e := range tmpEntries {
		t.Errorf("temp file left after interrupt: %s", e.Name())
	}
	// Rebuild should succeed.
	req.Output = filepath.Join(env.outDir, "rebuilt.tar")
	req.Workers = 2
	if _, err := Build(context.Background(), req); err != nil {
		t.Fatalf("rebuild after interrupt failed: %v", err)
	}
}

func TestBuildMissingContextFile(t *testing.T) {
	env := newBuildEnv(t)
	desc := env.writeDesc("FROM scratch\nCOPY missing.txt /a\n")
	req := &BuildRequest{
		DescriptionFile: desc,
		ContextDir:      env.ctxDir,
		Output:          filepath.Join(env.outDir, "x.tar"),
		Platforms:       []Platform{CurrentPlatform()},
		CacheDir:        env.cacheDir,
	}
	_, err := Build(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for missing context file")
	}
	if !strings.Contains(err.Error(), "missing.txt") {
		t.Errorf("error %q does not mention missing.txt", err.Error())
	}
}

func TestBuildOversizedLayer(t *testing.T) {
	env := newBuildEnv(t)
	env.writeFile("big.bin", strings.Repeat("x", 2000))
	desc := env.writeDesc("FROM scratch\nCOPY big.bin /big.bin\n")
	req := &BuildRequest{
		DescriptionFile: desc,
		ContextDir:      env.ctxDir,
		Output:          filepath.Join(env.outDir, "x.tar"),
		Platforms:       []Platform{CurrentPlatform()},
		CacheDir:        env.cacheDir,
		MaxLayerSize:    100,
	}
	_, err := Build(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for oversized layer")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error %q does not mention size limit", err.Error())
	}
}

func TestBuildWorkdir(t *testing.T) {
	env := newBuildEnv(t)
	desc := env.writeDesc("FROM scratch\nWORKDIR /app\n")
	_, out := env.build(desc)
	index, blobs := readArchive(t, out)
	mf := index.Manifests[0]
	var man oci.Manifest
	json.Unmarshal(blobs[mf.Digest.Hex()], &man)
	var cfg oci.ImageConfig
	json.Unmarshal(blobs[man.Config.Digest.Hex()], &cfg)
	if cfg.Config.WorkingDir != "/app" {
		t.Errorf("workingDir = %q, want /app", cfg.Config.WorkingDir)
	}
}

func TestBuildEnvLabel(t *testing.T) {
	env := newBuildEnv(t)
	desc := env.writeDesc("FROM scratch\nENV FOO=bar\nLABEL key=value\n")
	_, out := env.build(desc)
	index, blobs := readArchive(t, out)
	mf := index.Manifests[0]
	var man oci.Manifest
	json.Unmarshal(blobs[mf.Digest.Hex()], &man)
	var cfg oci.ImageConfig
	json.Unmarshal(blobs[man.Config.Digest.Hex()], &cfg)
	if len(cfg.Config.Env) != 1 || cfg.Config.Env[0] != "FOO=bar" {
		t.Errorf("env = %v", cfg.Config.Env)
	}
	if cfg.Config.Labels["key"] != "value" {
		t.Errorf("labels = %v", cfg.Config.Labels)
	}
}

// makeMinimalOCIArchive creates a minimal OCI archive tar with one platform.
func makeMinimalOCIArchive(t *testing.T, dir, platform string) string {
	t.Helper()
	layoutDir := filepath.Join(dir, "layout")
	if err := os.MkdirAll(filepath.Join(layoutDir, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	// oci-layout
	os.WriteFile(filepath.Join(layoutDir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644)
	// empty layer tar
	var layerBuf bytes.Buffer
	tw := tar.NewWriter(&layerBuf)
	tw.Close()
	layerData := layerBuf.Bytes()
	layerDigest := digest.FromBytes(layerData)
	os.WriteFile(filepath.Join(layoutDir, "blobs", "sha256", layerDigest.Hex()), layerData, 0o644)
	// config
	cfg := oci.NewImageConfig(oci.Platform{OS: "linux", Architecture: "amd64"}, time.Unix(0, 0))
	cfgData, cfgDigest, _ := oci.MarshalJSONDeterministic(cfg)
	os.WriteFile(filepath.Join(layoutDir, "blobs", "sha256", cfgDigest.Hex()), cfgData, 0o644)
	// manifest
	man := &oci.Manifest{
		SchemaVersion: 2,
		MediaType:     oci.MediaTypeManifest,
		Config:        oci.Descriptor{MediaType: oci.MediaTypeConfig, Digest: cfgDigest, Size: int64(len(cfgData))},
		Layers:        []oci.Descriptor{{MediaType: oci.MediaTypeLayerTar, Digest: layerDigest, Size: int64(len(layerData))}},
	}
	manData, manDigest, _ := oci.MarshalJSONDeterministic(man)
	os.WriteFile(filepath.Join(layoutDir, "blobs", "sha256", manDigest.Hex()), manData, 0o644)
	// index
	idx := &oci.Index{
		SchemaVersion: 2,
		MediaType:     oci.MediaTypeIndex,
		Manifests: []oci.Descriptor{{
			MediaType: oci.MediaTypeManifest, Digest: manDigest, Size: int64(len(manData)),
			Platform: &oci.Platform{OS: "linux", Architecture: "amd64"},
		}},
	}
	idxData, _ := json.Marshal(idx)
	os.WriteFile(filepath.Join(layoutDir, "index.json"), idxData, 0o644)
	// tar it
	tarPath := filepath.Join(dir, "base.tar")
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw2 := tar.NewWriter(f)
	filepath.Walk(layoutDir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(layoutDir, p)
		if fi.IsDir() {
			return nil
		}
		data, _ := os.ReadFile(p)
		hdr := &tar.Header{Name: rel, Mode: 0o644, Size: int64(len(data)), ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg}
		tw2.WriteHeader(hdr)
		tw2.Write(data)
		return nil
	})
	tw2.Close()
	return tarPath
}

func TestBuildMissingPlatform(t *testing.T) {
	env := newBuildEnv(t)
	env.writeFile("a.txt", "aaa")
	basePath := makeMinimalOCIArchive(t, env.outDir, "linux/amd64")
	desc := env.writeDesc("FROM mybase\nCOPY a.txt /a\n")
	req := &BuildRequest{
		DescriptionFile: desc,
		ContextDir:      env.ctxDir,
		Output:          filepath.Join(env.outDir, "x.tar"),
		Platforms:       []Platform{{OS: "linux", Arch: "arm64"}},
		CacheDir:        env.cacheDir,
		Bases:           map[string]string{"mybase": basePath},
	}
	_, err := Build(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for missing platform")
	}
	if !strings.Contains(err.Error(), "no manifest for platform") {
		t.Errorf("error %q does not mention missing platform", err.Error())
	}
}
