package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/cuixudong74/container-image-builder-go/pkg/archive"
	"github.com/cuixudong74/container-image-builder-go/pkg/buildfile"
	"github.com/cuixudong74/container-image-builder-go/pkg/image"
)

// layerRef identifies a produced layer blob.
type layerRef struct {
	Digest string
	Size   int64
}

// stageState carries everything a stage produces and dependents consume.
type stageState struct {
	name     string
	def      *buildfile.Stage
	fs       *vfs
	chainKey string // cache chain key after the last layer instruction
	layers   []layerRef
	cfg      configState
	history  []image.History

	done chan struct{}
	err  error
}

// configState accumulates the runtime config across instructions.
type configState struct {
	user       string
	env        []string
	envIdx     map[string]int
	entrypoint []string
	cmd        []string
	workdir    string
	labels     map[string]string
	ports      map[string]struct{}
}

func newConfigState() configState {
	return configState{
		envIdx: map[string]int{},
		labels: map[string]string{},
		ports:  map[string]struct{}{},
	}
}

func (c *configState) setEnv(k, v string) {
	kv := k + "=" + v
	if i, ok := c.envIdx[k]; ok {
		c.env[i] = kv
		return
	}
	c.envIdx[k] = len(c.env)
	c.env = append(c.env, kv)
}

// hashChain derives the next chain key.
func hashChain(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s\x00", len(p), p)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// canonicalCopy renders a COPY instruction in a canonical form for cache
// keys and history.
func canonicalCopy(ins *buildfile.Instruction) string {
	var b strings.Builder
	b.WriteString("COPY")
	if ins.CopyFrom != "" {
		b.WriteString(" --from=" + ins.CopyFrom)
	}
	if ins.CopyChmod != "" {
		b.WriteString(" --chmod=" + ins.CopyChmod)
	}
	for _, s := range ins.CopySrcs {
		b.WriteString(" " + s)
	}
	b.WriteString(" " + ins.CopyDest)
	return b.String()
}

// runStage executes one stage for one platform. It must be called after
// all dependency stageStates have completed (done closed, err checked).
func (e *Engine) runStage(ctx context.Context, st *stageState, plat image.Platform, deps map[string]*stageState) (retErr error) {
	defer func() {
		st.err = retErr
		close(st.done)
	}()
	fail := func(ins *buildfile.Instruction, op string, err error) error {
		line, kind := 0, ""
		if ins != nil {
			line, kind = ins.Line, ins.Kind.String()
		}
		return &StepError{Stage: st.name, Line: line, Op: op + kind, Err: err}
	}

	st.fs = newVFS()
	st.cfg = newConfigState()

	// FROM
	from := &st.def.From
	switch {
	case from.FromRef == "scratch":
		st.chainKey = hashChain("cib/v1", plat.OS, plat.Architecture, "FROM scratch")
	case strings.HasPrefix(from.FromRef, "oci:"):
		base, err := e.loadBase(strings.TrimPrefix(from.FromRef, "oci:"), plat)
		if err != nil {
			return fail(from, "", err)
		}
		st.fs = base.fs
		st.layers = base.layers
		st.cfg = base.cfg
		st.chainKey = hashChain("cib/v1", plat.OS, plat.Architecture, "FROM oci", base.manifestDigest)
	default:
		dep := deps[strings.ToLower(from.FromRef)]
		st.fs = dep.fs.clone()
		st.layers = append([]layerRef(nil), dep.layers...)
		st.cfg = dep.cfg.clone()
		st.chainKey = hashChain("cib/v1", plat.OS, plat.Architecture, "FROM stage", dep.chainKey)
	}
	st.history = append(st.history, image.History{
		Created:   e.epoch,
		CreatedBy: "FROM " + from.FromRef,
	})

	for i := range st.def.Body {
		ins := &st.def.Body[i]
		if err := ctx.Err(); err != nil {
			return fail(ins, "cancelled before ", err)
		}
		switch ins.Kind {
		case buildfile.KindCopy:
			if err := e.execCopy(ctx, st, ins, deps); err != nil {
				return fail(ins, "", err)
			}
		case buildfile.KindEnv:
			for _, kv := range ins.Pairs {
				st.cfg.setEnv(kv.Key, kv.Value)
			}
			e.addHistory(st, "ENV "+joinPairs(ins.Pairs), true)
		case buildfile.KindLabel:
			for _, kv := range ins.Pairs {
				st.cfg.labels[kv.Key] = kv.Value
			}
			e.addHistory(st, "LABEL "+joinPairs(ins.Pairs), true)
		case buildfile.KindWorkdir:
			st.cfg.workdir = cleanPath(ins.Value)
			e.addHistory(st, "WORKDIR "+ins.Value, true)
		case buildfile.KindUser:
			st.cfg.user = ins.Value
			e.addHistory(st, "USER "+ins.Value, true)
		case buildfile.KindCmd:
			st.cfg.cmd = argvOf(ins)
			e.addHistory(st, "CMD "+renderArgv(ins), true)
		case buildfile.KindEntrypoint:
			st.cfg.entrypoint = argvOf(ins)
			e.addHistory(st, "ENTRYPOINT "+renderArgv(ins), true)
		case buildfile.KindExpose:
			for _, p := range ins.Ports {
				if !strings.Contains(p, "/") {
					p += "/tcp"
				}
				st.cfg.ports[p] = struct{}{}
			}
			e.addHistory(st, "EXPOSE "+strings.Join(ins.Ports, " "), true)
		default:
			return fail(ins, "unsupported instruction ", fmt.Errorf("%s not supported", ins.Kind))
		}
	}
	return nil
}

func (e *Engine) addHistory(st *stageState, by string, empty bool) {
	st.history = append(st.history, image.History{Created: e.epoch, CreatedBy: by, EmptyLayer: empty})
}

// execCopy plans and executes one COPY instruction, consulting the cache.
func (e *Engine) execCopy(ctx context.Context, st *stageState, ins *buildfile.Instruction, deps map[string]*stageState) error {
	var srcStage *stageState
	if ins.CopyFrom != "" {
		srcStage = deps[strings.ToLower(ins.CopyFrom)]
	}
	plan, err := planCopy(ins, st.cfg.workdir, e.bctx, srcStage)
	if err != nil {
		return err
	}
	canon := canonicalCopy(ins)
	key := hashChain(st.chainKey, canon, plan.inputDigest())

	digest, ok, err := e.cache.Get(key)
	if err != nil {
		return fmt.Errorf("cache lookup: %w", err)
	}
	var size int64
	if ok {
		size, err = e.cache.BlobSize(digest)
		if err != nil {
			return err
		}
	} else {
		entries := plan.toArchiveEntries()
		byName := plan.plannedByName()
		pr, pw := io.Pipe()
		go func() {
			w := &ctxWriter{ctx: ctx, w: pw}
			pw.CloseWithError(archive.WriteTar(w, e.epoch, entries, openPlanned(byName)))
		}()
		digest, err = e.cache.Put(key, &limitReader{r: pr, max: e.maxLayer})
		if err != nil {
			pr.CloseWithError(fmt.Errorf("aborted: %w", err))
			return err
		}
		size, err = e.cache.BlobSize(digest)
		if err != nil {
			return err
		}
	}
	st.layers = append(st.layers, layerRef{Digest: digest, Size: size})
	st.chainKey = key

	// Merge copied nodes into the stage vfs.
	for _, pe := range plan.entries {
		st.fs.ensureParents(pe.name)
		st.fs.set(pe.name, &vfsNode{
			Mode:   pe.mode,
			IsDir:  pe.isDir,
			Link:   pe.link,
			Size:   pe.size,
			Digest: pe.digest,
			Src:    pe.src,
		})
	}
	e.addHistory(st, canon, false)
	return nil
}

// ctxWriter fails writes once ctx is done.
type ctxWriter struct {
	ctx context.Context
	w   io.Writer
}

func (c *ctxWriter) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.w.Write(p)
}

// limitReader errors out once more than max bytes pass through.
type limitReader struct {
	r   io.Reader
	max int64
	n   int64
}

func (l *limitReader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	l.n += int64(n)
	if l.max > 0 && l.n > l.max {
		return n, fmt.Errorf("layer exceeds maximum size of %d bytes", l.max)
	}
	return n, err
}

func joinPairs(pairs []buildfile.KV) string {
	var parts []string
	for _, kv := range pairs {
		parts = append(parts, kv.Key+"="+kv.Value)
	}
	return strings.Join(parts, " ")
}

func argvOf(ins *buildfile.Instruction) []string {
	if ins.Argv != nil {
		return ins.Argv
	}
	return []string{"/bin/sh", "-c", ins.ShellForm}
}

func renderArgv(ins *buildfile.Instruction) string {
	if ins.Argv != nil {
		return "[" + strings.Join(ins.Argv, ",") + "]"
	}
	return ins.ShellForm
}

func (v *vfs) clone() *vfs {
	nv := newVFS()
	for k, n := range v.nodes {
		c := *n
		nv.nodes[k] = &c
	}
	return nv
}

func (c configState) clone() configState {
	nv := configState{
		user:       c.user,
		workdir:    c.workdir,
		env:        append([]string(nil), c.env...),
		envIdx:     map[string]int{},
		entrypoint: append([]string(nil), c.entrypoint...),
		cmd:        append([]string(nil), c.cmd...),
		labels:     map[string]string{},
		ports:      map[string]struct{}{},
	}
	for k, v := range c.envIdx {
		nv.envIdx[k] = v
	}
	for k, v := range c.labels {
		nv.labels[k] = v
	}
	for k := range c.ports {
		nv.ports[k] = struct{}{}
	}
	return nv
}
