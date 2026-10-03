// Package engine orchestrates multi-stage, multi-platform image builds:
// it schedules stages as a DAG, computes per-instruction cache keys,
// reuses cached layers, and emits an OCI image layout.
package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cuixudong74/container-image-builder-go/pkg/buildctx"
	"github.com/cuixudong74/container-image-builder-go/pkg/buildfile"
	"github.com/cuixudong74/container-image-builder-go/pkg/cache"
	"github.com/cuixudong74/container-image-builder-go/pkg/image"
)

// DefaultMaxLayerSize bounds a single layer (64 GiB).
const DefaultMaxLayerSize = 64 << 30

// Options configures one build.
type Options struct {
	BuildfilePath string    // path to the build description file
	ContextDir    string    // build context directory
	CacheDir      string    // layer cache directory (shared across builds)
	OutputDir     string    // output OCI layout directory
	Platforms     []string  // "os/arch" list; empty means linux/amd64
	Epoch         time.Time // fixed timestamp for reproducibility; zero uses Unix epoch
	MaxLayerSize  int64     // bytes; 0 means DefaultMaxLayerSize
	IgnoreFile    string    // name of ignore file inside context (".cibignore" when empty)
}

// Result describes a completed build.
type Result struct {
	IndexDigest string            // digest of the top-level image index
	Manifests   map[string]string // platform -> manifest digest
	OutputDir   string
}

// Engine runs builds.
type Engine struct {
	opts     Options
	epoch    time.Time
	cache    *cache.Cache
	bctx     *buildctx.Context
	maxLayer int64
	tmpDir   string // per-build scratch (base image extraction)

	blobMu  sync.Mutex
	blobs   map[string]struct{} // blobs referenced by the output layout
	blobSrc map[string]string   // digest -> host path for blobs outside the cache (base image layers)
}

// addOutputBlob records a blob digest that must appear in the output.
func (e *Engine) addOutputBlob(digest string) {
	e.blobMu.Lock()
	e.blobs[digest] = struct{}{}
	e.blobMu.Unlock()
}

// outputBlobs returns the sorted list of recorded blob digests.
func (e *Engine) outputBlobs() []string {
	e.blobMu.Lock()
	out := make([]string, 0, len(e.blobs))
	for d := range e.blobs {
		out = append(out, d)
	}
	e.blobMu.Unlock()
	sort.Strings(out)
	return out
}

// New validates opts and returns an Engine.
func New(opts Options) (*Engine, error) {
	if opts.BuildfilePath == "" {
		return nil, fmt.Errorf("buildfile path is required")
	}
	if opts.ContextDir == "" {
		return nil, fmt.Errorf("context dir is required")
	}
	if opts.CacheDir == "" {
		return nil, fmt.Errorf("cache dir is required")
	}
	if opts.OutputDir == "" {
		return nil, fmt.Errorf("output dir is required")
	}
	e := &Engine{opts: opts, epoch: opts.Epoch, maxLayer: opts.MaxLayerSize}
	if e.epoch.IsZero() {
		e.epoch = time.Unix(0, 0).UTC()
	}
	if e.maxLayer <= 0 {
		e.maxLayer = DefaultMaxLayerSize
	}
	if len(e.opts.Platforms) == 0 {
		e.opts.Platforms = []string{"linux/amd64"}
	}
	for _, p := range e.opts.Platforms {
		if _, err := parsePlatform(p); err != nil {
			return nil, err
		}
	}
	return e, nil
}

// parsePlatform parses "os/arch" strictly.
func parsePlatform(s string) (image.Platform, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return image.Platform{}, fmt.Errorf("invalid platform %q: want os/arch", s)
	}
	return image.Platform{OS: parts[0], Architecture: parts[1]}, nil
}

// Build executes the build. It is safe to cancel ctx at any time: the
// cache is left consistent and no partial output is committed.
func (e *Engine) Build(ctx context.Context) (*Result, error) {
	f, err := os.Open(e.opts.BuildfilePath)
	if err != nil {
		return nil, fmt.Errorf("open buildfile: %w", err)
	}
	defer f.Close()
	bf, err := buildfile.Parse(f)
	if err != nil {
		return nil, err
	}

	ignoreFile := e.opts.IgnoreFile
	if ignoreFile == "" {
		ignoreFile = ".cibignore"
	}
	e.bctx, err = buildctx.New(e.opts.ContextDir, ignoreFile, nil)
	if err != nil {
		return nil, err
	}
	e.cache, err = cache.Open(e.opts.CacheDir)
	if err != nil {
		return nil, err
	}

	tmp, err := os.MkdirTemp(filepath.Dir(e.opts.OutputDir), ".cib-build-*")
	if err != nil {
		return nil, fmt.Errorf("create scratch dir: %w", err)
	}
	e.tmpDir = tmp
	e.blobs = map[string]struct{}{}
	e.blobSrc = map[string]string{}
	defer os.RemoveAll(tmp) // cancelled or failed builds leave nothing behind

	// Build every platform concurrently; stages within a platform also run
	// concurrently as a DAG. First failure cancels everything.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type platResult struct {
		plat string
		desc image.Descriptor
	}
	results := make([]platResult, len(e.opts.Platforms))
	var wg sync.WaitGroup
	errCh := make(chan error, len(e.opts.Platforms))
	for i, ps := range e.opts.Platforms {
		plat, _ := parsePlatform(ps)
		wg.Add(1)
		go func(i int, ps string, plat image.Platform) {
			defer wg.Done()
			desc, err := e.buildPlatform(ctx, bf, plat)
			if err != nil {
				errCh <- err
				cancel()
				return
			}
			results[i] = platResult{plat: ps, desc: desc}
		}(i, ps, plat)
	}
	wg.Wait()
	close(errCh)
	if err := <-errCh; err != nil {
		return nil, err
	}

	// Assemble the image index, manifests sorted by platform string for
	// byte-stable output.
	sort.Slice(results, func(i, j int) bool { return results[i].plat < results[j].plat })
	idx := image.Index{SchemaVersion: 2, MediaType: image.MediaTypeImageIndex}
	manifests := map[string]string{}
	for _, r := range results {
		idx.Manifests = append(idx.Manifests, r.desc)
		manifests[r.plat] = r.desc.Digest
	}
	indexDigest, err := e.writeOutput(idx)
	if err != nil {
		return nil, err
	}
	return &Result{IndexDigest: indexDigest, Manifests: manifests, OutputDir: e.opts.OutputDir}, nil
}

// buildPlatform runs the full stage DAG for one platform and returns the
// manifest descriptor of the final stage.
func (e *Engine) buildPlatform(ctx context.Context, bf *buildfile.Buildfile, plat image.Platform) (image.Descriptor, error) {
	states := make([]*stageState, len(bf.Stages))
	byName := map[string]*stageState{}
	for i, def := range bf.Stages {
		st := &stageState{name: def.Name, def: def, done: make(chan struct{})}
		states[i] = st
		byName[strings.ToLower(def.Name)] = st
	}

	errCh := make(chan error, len(states))
	for _, st := range states {
		deps := depStages(st.def, byName)
		go func(st *stageState, deps map[string]*stageState) {
			// Wait for dependencies; abort early on cancellation.
			for _, d := range deps {
				select {
				case <-d.done:
				case <-ctx.Done():
					st.err = ctx.Err()
					close(st.done)
					errCh <- st.err
					return
				}
			}
			for _, d := range deps {
				if d.err != nil {
					st.err = fmt.Errorf("dependency stage %s failed: %w", d.name, d.err)
					close(st.done)
					errCh <- st.err
					return
				}
			}
			if err := e.runStage(ctx, st, plat, deps); err != nil {
				errCh <- err
			}
		}(st, deps)
	}

	final := states[len(states)-1]
	<-final.done
	if final.err != nil {
		return image.Descriptor{}, final.err
	}
	select {
	case err := <-errCh:
		return image.Descriptor{}, err
	default:
	}
	return e.assembleManifest(states[len(states)-1], plat)
}

// depStages returns the stageStates a stage depends on, keyed by
// lower-cased stage name.
func depStages(def *buildfile.Stage, byName map[string]*stageState) map[string]*stageState {
	deps := map[string]*stageState{}
	ref := def.From.FromRef
	if ref != "scratch" && !strings.HasPrefix(ref, "oci:") {
		deps[strings.ToLower(ref)] = byName[strings.ToLower(ref)]
	}
	for _, ins := range def.Body {
		if ins.Kind == buildfile.KindCopy && ins.CopyFrom != "" {
			deps[strings.ToLower(ins.CopyFrom)] = byName[strings.ToLower(ins.CopyFrom)]
		}
	}
	return deps
}

// StepError identifies the stage and instruction line that failed.
type StepError struct {
	Stage string
	Line  int
	Op    string
	Err   error
}

func (e *StepError) Error() string {
	op := strings.TrimSpace(e.Op)
	if e.Line > 0 {
		return "stage " + strconv.Quote(e.Stage) + " line " + itoa(e.Line) + " (" + op + "): " + e.Err.Error()
	}
	return "stage " + strconv.Quote(e.Stage) + " (" + op + "): " + e.Err.Error()
}

func (e *StepError) Unwrap() error { return e.Err }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	p := len(buf)
	for i > 0 {
		p--
		buf[p] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[p:])
}
