package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cuixudong74/container-image-builder-go/pkg/image"
)

// fixture creates a build workspace and returns a function to run builds.
type fixture struct {
	t      *testing.T
	dir    string
	ctxDir string
	cache  string
	outN   atomic.Int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{t: t, dir: dir, ctxDir: filepath.Join(dir, "ctx"), cache: filepath.Join(dir, "cache")}
	if err := os.MkdirAll(f.ctxDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) writeCtx(name, content string) {
	f.t.Helper()
	p := filepath.Join(f.ctxDir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) writeBuildfile(content string) string {
	f.t.Helper()
	p := filepath.Join(f.dir, "Containerfile")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return p
}

type buildOpt func(*Options)

func withPlatforms(p ...string) buildOpt { return func(o *Options) { o.Platforms = p } }
func withMaxLayer(n int64) buildOpt      { return func(o *Options) { o.MaxLayerSize = n } }
func withCacheDir(d string) buildOpt     { return func(o *Options) { o.CacheDir = d } }
func withOutputDir(d string) buildOpt    { return func(o *Options) { o.OutputDir = d } }

func (f *fixture) build(ctx context.Context, buildfile string, opts ...buildOpt) (*Result, error) {
	f.t.Helper()
	n := f.outN.Add(1)
	o := Options{
		BuildfilePath: buildfile,
		ContextDir:    f.ctxDir,
		CacheDir:      f.cache,
		OutputDir:     filepath.Join(f.dir, fmt.Sprintf("out-%d", n)),
		Epoch:         time.Unix(0, 0).UTC(),
	}
	for _, fn := range opts {
		fn(&o)
	}
	eng, err := New(o)
	if err != nil {
		return nil, err
	}
	return eng.Build(ctx)
}

func readIndex(t *testing.T, dir string) image.Index {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx image.Index
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatal(err)
	}
	return idx
}

func TestCacheHitAndInvalidation(t *testing.T) {
	f := newFixture(t)
	f.writeCtx("app/code.txt", "v1")
	f.writeCtx("app/other.txt", "untouched")
	bf := f.writeBuildfile(`FROM scratch
COPY app /app
COPY app/other.txt /other.txt
`)

	r1, err := f.build(context.Background(), bf)
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild unchanged: identical index digest (cache hit, reproducible).
	r2, err := f.build(context.Background(), bf)
	if err != nil {
		t.Fatal(err)
	}
	if r1.IndexDigest != r2.IndexDigest {
		t.Fatalf("unchanged input produced different digests: %s vs %s", r1.IndexDigest, r2.IndexDigest)
	}

	// Change one file: downstream cache must invalidate.
	f.writeCtx("app/code.txt", "v2")
	r3, err := f.build(context.Background(), bf)
	if err != nil {
		t.Fatal(err)
	}
	if r3.IndexDigest == r1.IndexDigest {
		t.Fatal("content change did not invalidate cache")
	}

	// Revert: original digest must come back (cache reuse across builds).
	f.writeCtx("app/code.txt", "v1")
	r4, err := f.build(context.Background(), bf)
	if err != nil {
		t.Fatal(err)
	}
	if r4.IndexDigest != r1.IndexDigest {
		t.Fatalf("reverted content did not hit cache: %s vs %s", r4.IndexDigest, r1.IndexDigest)
	}
}

func TestUpstreamChangeInvalidatesDownstream(t *testing.T) {
	f := newFixture(t)
	f.writeCtx("base.txt", "base-v1")
	f.writeCtx("top.txt", "top-v1")
	bf := f.writeBuildfile(`FROM scratch
COPY base.txt /base.txt
COPY top.txt /top.txt
`)
	r1, err := f.build(context.Background(), bf)
	if err != nil {
		t.Fatal(err)
	}
	// Change only the FIRST copy: the second layer's cache key chains on
	// the first, so the manifest must change even though top.txt is same.
	f.writeCtx("base.txt", "base-v2")
	r2, err := f.build(context.Background(), bf)
	if err != nil {
		t.Fatal(err)
	}
	if r1.IndexDigest == r2.IndexDigest {
		t.Fatal("upstream change did not invalidate downstream")
	}
	// But the top.txt layer blob itself must be reused: same content,
	// same digest. Verify via manifest layers.
	m1 := finalManifestLayers(t, r1.OutputDir)
	m2 := finalManifestLayers(t, r2.OutputDir)
	if len(m1) != 2 || len(m2) != 2 {
		t.Fatalf("want 2 layers each, got %v and %v", m1, m2)
	}
	if m1[0] == m2[0] {
		t.Fatal("first layer should have changed")
	}
	if m1[1] != m2[1] {
		t.Fatal("second layer content unchanged, digest must be identical")
	}
}

func finalManifestLayers(t *testing.T, outDir string) []string {
	t.Helper()
	idx := readIndex(t, outDir)
	if len(idx.Manifests) != 1 {
		t.Fatalf("want 1 manifest, got %d", len(idx.Manifests))
	}
	data, err := os.ReadFile(filepath.Join(outDir, "blobs", "sha256",
		strings.TrimPrefix(idx.Manifests[0].Digest, "sha256:")))
	if err != nil {
		t.Fatal(err)
	}
	var m image.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	var layers []string
	for _, l := range m.Layers {
		layers = append(layers, l.Digest)
	}
	return layers
}

func TestMultiStageCopy(t *testing.T) {
	f := newFixture(t)
	f.writeCtx("src/main.sh", "#!/bin/sh\necho hi\n")
	bf := f.writeBuildfile(`FROM scratch AS build
COPY src /src

FROM scratch AS final
COPY --from=build /src/main.sh /bin/main
CMD ["/bin/main"]
`)
	res, err := f.build(context.Background(), bf)
	if err != nil {
		t.Fatal(err)
	}
	layers := finalManifestLayers(t, res.OutputDir)
	if len(layers) != 1 {
		t.Fatalf("final stage should have 1 layer, got %d", len(layers))
	}
	// The layer must contain bin/main with the content.
	blob := filepath.Join(res.OutputDir, "blobs", "sha256", strings.TrimPrefix(layers[0], "sha256:"))
	data, err := os.ReadFile(blob)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "echo hi") {
		t.Fatal("layer does not contain copied content")
	}
}

func TestMultiPlatformIndex(t *testing.T) {
	f := newFixture(t)
	f.writeCtx("a.txt", "a")
	bf := f.writeBuildfile("FROM scratch\nCOPY a.txt /a.txt\n")
	res, err := f.build(context.Background(), bf, withPlatforms("linux/amd64", "linux/arm64", "linux/riscv64"))
	if err != nil {
		t.Fatal(err)
	}
	idx := readIndex(t, res.OutputDir)
	if len(idx.Manifests) != 3 {
		t.Fatalf("want 3 manifests, got %d", len(idx.Manifests))
	}
	seen := map[string]bool{}
	for _, m := range idx.Manifests {
		if m.Platform == nil {
			t.Fatal("manifest missing platform")
		}
		seen[m.Platform.OS+"/"+m.Platform.Architecture] = true
	}
	for _, p := range []string{"linux/amd64", "linux/arm64", "linux/riscv64"} {
		if !seen[p] {
			t.Fatalf("platform %s missing from index", p)
		}
		if res.Manifests[p] == "" {
			t.Fatalf("result missing platform %s", p)
		}
	}
	// Manifests sorted by platform string for byte stability.
	if idx.Manifests[0].Platform.Architecture != "amd64" {
		t.Fatalf("manifests not sorted: %+v", idx.Manifests[0].Platform)
	}
}

func TestReproducibleAcrossDirsAndTimes(t *testing.T) {
	build := func() string {
		f := newFixture(t)
		f.writeCtx("x/data.bin", "some data")
		bf := f.writeBuildfile("FROM scratch\nCOPY x /x\n")
		// Touch files to random-ish mtimes; digest must not depend on them.
		past := time.Now().Add(-1000 * time.Hour)
		_ = os.Chtimes(filepath.Join(f.ctxDir, "x", "data.bin"), past, past)
		res, err := f.build(context.Background(), bf)
		if err != nil {
			t.Fatal(err)
		}
		return res.IndexDigest
	}
	d1 := build()
	time.Sleep(10 * time.Millisecond)
	d2 := build()
	if d1 != d2 {
		t.Fatalf("identical input built differently: %s vs %s", d1, d2)
	}
}

func TestConcurrentBuildsSharedCache(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 4; i++ {
		f.writeCtx(fmt.Sprintf("f%d.txt", i), strings.Repeat("x", 100))
	}
	bf := f.writeBuildfile(`FROM scratch
COPY f0.txt /0
COPY f1.txt /1
COPY f2.txt /2
COPY f3.txt /3
`)
	const n = 8
	results := make([]*Result, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each build gets its own output dir but shares the cache.
			res, err := f.build(context.Background(), bf,
				withOutputDir(filepath.Join(f.dir, fmt.Sprintf("shared-out-%d", i))))
			results[i] = res
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("concurrent build %d failed: %v", i, errs[i])
		}
		if results[i].IndexDigest != results[0].IndexDigest {
			t.Fatalf("concurrent builds disagree: %s vs %s", results[i].IndexDigest, results[0].IndexDigest)
		}
	}
}

func TestCancellationLeavesConsistentState(t *testing.T) {
	f := newFixture(t)
	// A largish file so the build takes a moment.
	f.writeCtx("big.bin", strings.Repeat("z", 4<<20))
	bf := f.writeBuildfile("FROM scratch\nCOPY big.bin /big.bin\nCOPY big.bin /big2.bin\n")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	_, err := f.build(ctx, bf)
	if err == nil {
		t.Fatal("cancelled build should fail")
	}
	// No output dir committed.
	if _, statErr := os.Stat(filepath.Join(f.dir, "out-1")); !os.IsNotExist(statErr) {
		t.Fatal("cancelled build left an output dir")
	}
	// No scratch dirs left behind.
	entries, _ := os.ReadDir(f.dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".cib-build-") {
			t.Fatalf("scratch dir left behind: %s", e.Name())
		}
	}
	// A fresh build with the same cache must succeed and be consistent.
	res, err := f.build(context.Background(), bf)
	if err != nil {
		t.Fatalf("rebuild after cancel failed: %v", err)
	}
	res2, err := f.build(context.Background(), bf)
	if err != nil {
		t.Fatal(err)
	}
	if res.IndexDigest != res2.IndexDigest {
		t.Fatal("post-cancel builds not reproducible")
	}
}

func TestErrorPaths(t *testing.T) {
	f := newFixture(t)
	f.writeCtx("ok.txt", "ok")

	// Missing context file: error names stage, line and instruction.
	bf := f.writeBuildfile("FROM scratch\nCOPY missing.txt /x\n")
	_, err := f.build(context.Background(), bf)
	if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "COPY") {
		t.Fatalf("missing file error not locatable: %v", err)
	}

	// Corrupt buildfile.
	bad := f.writeBuildfile("FROM scratch\nBOGUS x\n")
	_, err = f.build(context.Background(), bad)
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("parse error not locatable: %v", err)
	}

	// Oversized layer.
	big := f.writeBuildfile("FROM scratch\nCOPY ok.txt /ok\n")
	_, err = f.build(context.Background(), big, withMaxLayer(2))
	if err == nil || !strings.Contains(err.Error(), "maximum size") {
		t.Fatalf("oversize layer error: %v", err)
	}

	// COPY --from a stage path that does not exist.
	badPath := f.writeBuildfile("FROM scratch AS a\nCOPY ok.txt /ok\nFROM scratch\nCOPY --from=a /nope /x\n")
	_, err = f.build(context.Background(), badPath)
	if err == nil || !strings.Contains(err.Error(), "not found in stage") {
		t.Fatalf("stage path error: %v", err)
	}
}

func TestFromOCIAndMissingPlatform(t *testing.T) {
	// Build a base image first.
	f1 := newFixture(t)
	f1.writeCtx("base.txt", "base-content")
	bf1 := f1.writeBuildfile("FROM scratch\nCOPY base.txt /base.txt\nENV FROM_BASE=yes\n")
	base, err := f1.build(context.Background(), bf1, withPlatforms("linux/amd64"))
	if err != nil {
		t.Fatal(err)
	}

	// Chain: use it as base for linux/amd64.
	f2 := newFixture(t)
	f2.writeCtx("app.txt", "app")
	bf2 := f2.writeBuildfile(fmt.Sprintf("FROM oci:%s\nCOPY app.txt /app.txt\n", base.OutputDir))
	res, err := f2.build(context.Background(), bf2, withPlatforms("linux/amd64"), withCacheDir(filepath.Join(t.TempDir(), "c")))
	if err != nil {
		t.Fatal(err)
	}
	layers := finalManifestLayers(t, res.OutputDir)
	if len(layers) != 2 {
		t.Fatalf("chained image should have 2 layers, got %d", len(layers))
	}

	// Request a platform the base lacks: must fail clearly.
	_, err = f2.build(context.Background(), bf2, withPlatforms("linux/arm64"), withCacheDir(filepath.Join(t.TempDir(), "c")))
	if err == nil || !strings.Contains(err.Error(), "no manifest for platform linux/arm64") {
		t.Fatalf("missing platform error: %v", err)
	}
}

func TestIgnoreFileExcludesContent(t *testing.T) {
	f := newFixture(t)
	f.writeCtx("keep.txt", "keep")
	f.writeCtx("secret.key", "secret")
	f.writeCtx(".cibignore", "secret.key\n")
	bf := f.writeBuildfile("FROM scratch\nCOPY keep.txt /keep.txt\n")
	res, err := f.build(context.Background(), bf)
	if err != nil {
		t.Fatal(err)
	}
	// COPY of an ignored file must fail as not-found.
	bad := f.writeBuildfile("FROM scratch\nCOPY secret.key /key\n")
	_, err = f.build(context.Background(), bad)
	if err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("ignored file copy should fail: %v", err)
	}
	_ = res
}

func TestDeterministicConfigBytes(t *testing.T) {
	f := newFixture(t)
	f.writeCtx("a.txt", "a")
	bf := f.writeBuildfile(`FROM scratch
COPY a.txt /a.txt
ENV Z=1 A=2
LABEL x=y
EXPOSE 80
WORKDIR /app
USER 1000
`)
	r1, err := f.build(context.Background(), bf)
	if err != nil {
		t.Fatal(err)
	}
	// New cache dir: config/manifest bytes must still be identical.
	r2, err := f.build(context.Background(), bf, withCacheDir(filepath.Join(t.TempDir(), "fresh-cache")))
	if err != nil {
		t.Fatal(err)
	}
	if r1.IndexDigest != r2.IndexDigest {
		t.Fatal("cold-cache build differs from warm-cache build")
	}
}
