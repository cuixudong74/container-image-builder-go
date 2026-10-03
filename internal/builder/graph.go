package builder

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/cuixudong74/container-image-builder-go/internal/buildctx"
	"github.com/cuixudong74/container-image-builder-go/internal/cache"
	"github.com/cuixudong74/container-image-builder-go/internal/description"
)

// Graph schedules stage builds across platforms with dependency awareness.
type Graph struct {
	desc      *description.BuildDescription
	platforms []Platform
	ctx       *buildctx.Context
	cache     *cache.Cache
	bases     map[string]string
	opts      BuildOptions
}

// NewGraph creates a build graph.
func NewGraph(desc *description.BuildDescription, platforms []Platform,
	ctx *buildctx.Context, c *cache.Cache, bases map[string]string, opts BuildOptions) *Graph {
	return &Graph{
		desc: desc, platforms: platforms, ctx: ctx, cache: c, bases: bases, opts: opts,
	}
}

func stageKey(stageIdx int, p Platform) string {
	return fmt.Sprintf("%d|%s", stageIdx, p.String())
}

// Build runs all stages for all platforms, parallelizing independent stages.
// On the first error the context is cancelled and in-flight builds stop.
func (g *Graph) Build(ctx context.Context) (map[string]*StageResult, error) {
	if err := g.validate(); err != nil {
		return nil, err
	}

	workDir, err := os.MkdirTemp(g.cache.TmpDir(), "build-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(workDir)

	store := newStageStore(g.desc.Stages, g.platforms)

	gctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sem := make(chan struct{}, g.opts.Workers)
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once

	for i := range g.desc.Stages {
		for _, p := range g.platforms {
			wg.Add(1)
			go func(stageIdx int, plat Platform) {
				defer wg.Done()
				// Wait for COPY --from dependencies (same platform).
				for _, dep := range g.depsOf(stageIdx) {
					if !store.wait(gctx, dep, plat) {
						return
					}
				}
				// Acquire a worker slot.
				select {
				case sem <- struct{}{}:
				case <-gctx.Done():
					return
				}
				defer func() { <-sem }()
				if gctx.Err() != nil {
					return
				}

				rootfs := filepath.Join(workDir, fmt.Sprintf("stage-%d-%s", stageIdx, strings.ReplaceAll(plat.String(), "/", "-")))
				if err := os.MkdirAll(rootfs, 0o755); err != nil {
					errOnce.Do(func() { firstErr = err; cancel() })
					return
				}
				exec := NewExecutor(g.desc, stageIdx, plat, g.ctx, g.cache, g.bases, g.opts, store)
				res, err := exec.Run(rootfs)
				if err != nil {
					errOnce.Do(func() { firstErr = err; cancel() })
					return
				}
				store.put(stageIdx, plat, res)
			}(i, p)
		}
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return store.snapshot(), nil
}

// stageStore holds completed stage results in a concurrency-safe way.
type stageStore struct {
	mu      sync.Mutex
	results map[string]*StageResult
	done    map[string]chan struct{}
}

func newStageStore(stages []description.Stage, platforms []Platform) *stageStore {
	s := &stageStore{results: map[string]*StageResult{}, done: map[string]chan struct{}{}}
	for i := range stages {
		for _, p := range platforms {
			s.done[stageKey(i, p)] = make(chan struct{})
		}
	}
	return s
}

func (s *stageStore) put(stageIdx int, p Platform, res *StageResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := stageKey(stageIdx, p)
	if _, ok := s.results[k]; !ok {
		s.results[k] = res
		close(s.done[k])
	}
}

// wait blocks until the given stage completes or ctx is cancelled.
func (s *stageStore) wait(ctx context.Context, stageIdx int, p Platform) bool {
	select {
	case <-s.done[stageKey(stageIdx, p)]:
		return true
	case <-ctx.Done():
		return false
	}
}

// byIndex returns a completed stage result by index (same platform).
func (s *stageStore) byIndex(stageIdx int, p Platform) *StageResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.results[stageKey(stageIdx, p)]
}

// byName returns a completed stage result by name (same platform).
func (s *stageStore) byName(name string, p Platform) *StageResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, res := range s.results {
		if res.StageName == name && k[len(k)-len(p.String()):] == p.String() {
			return res
		}
	}
	return nil
}

func (s *stageStore) snapshot() map[string]*StageResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*StageResult, len(s.results))
	for k, v := range s.results {
		out[k] = v
	}
	return out
}

// validate checks stage references and names.
func (g *Graph) validate() error {
	names := map[string]bool{}
	for i, st := range g.desc.Stages {
		if st.Name != "" {
			if names[st.Name] {
				return description.NewStageError(st.Name, i, 0, "FROM", "duplicate stage name %q", st.Name)
			}
			names[st.Name] = true
		}
		if st.Platform != "" && !strings.Contains(st.Platform, "$") {
			if _, err := ParsePlatform(st.Platform); err != nil {
				return description.NewStageError(st.Name, i, 0, "FROM", "invalid --platform %q: %s", st.Platform, err)
			}
		}
	}
	for i, st := range g.desc.Stages {
		for _, instr := range st.Instructions {
			if instr.Kind != description.InstCopy && instr.Kind != description.InstAdd {
				continue
			}
			ca, ok := instr.Args["copy"].(*description.CopyArgs)
			if !ok || ca.From == "" {
				continue
			}
			ref := ca.From
			if idx, err := strconv.Atoi(ref); err == nil {
				if idx < 0 || idx >= len(g.desc.Stages) {
					return description.NewStageError(st.Name, i, instr.Line, "COPY",
						"--from=%s references stage index %d which does not exist (have %d stages)", ref, idx, len(g.desc.Stages))
				}
				continue
			}
			if !names[ref] {
				return description.NewStageError(st.Name, i, instr.Line, "COPY",
					"--from=%s references stage %q which does not exist", ref, ref)
			}
		}
	}
	return nil
}

// depsOf returns the stage indices that stageIdx copies from.
func (g *Graph) depsOf(stageIdx int) []int {
	seen := map[int]bool{}
	var out []int
	st := g.desc.Stages[stageIdx]
	for _, instr := range st.Instructions {
		if instr.Kind != description.InstCopy && instr.Kind != description.InstAdd {
			continue
		}
		ca, ok := instr.Args["copy"].(*description.CopyArgs)
		if !ok || ca.From == "" {
			continue
		}
		ref := ca.From
		if idx, err := strconv.Atoi(ref); err == nil {
			if idx >= 0 && idx < len(g.desc.Stages) && idx != stageIdx && !seen[idx] {
				seen[idx] = true
				out = append(out, idx)
			}
			continue
		}
		for j, s := range g.desc.Stages {
			if s.Name == ref && j != stageIdx && !seen[j] {
				seen[j] = true
				out = append(out, j)
			}
		}
	}
	sort.Ints(out)
	return out
}
