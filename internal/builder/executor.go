package builder

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cuixudong74/container-image-builder-go/internal/buildctx"
	"github.com/cuixudong74/container-image-builder-go/internal/cache"
	"github.com/cuixudong74/container-image-builder-go/internal/description"
	"github.com/cuixudong74/container-image-builder-go/internal/digest"
	"github.com/cuixudong74/container-image-builder-go/internal/oci"
)

// BuildOptions controls a build.
type BuildOptions struct {
	Epoch        time.Time
	MaxLayerSize int64 // 0 = unlimited
	MaxFileSize  int64 // 0 = unlimited
	Workers      int
}

// StageResult is the output of building one stage for one platform.
type StageResult struct {
	StageName      string
	StageIndex     int
	Platform       Platform
	Config         *oci.ImageConfig
	Manifest       *oci.Manifest
	ConfigDigest   digest.Digest
	ManifestDigest digest.Digest
	ChainKey       digest.Digest
	Rootfs         string
}

// Executor builds a single stage for a single platform.
type Executor struct {
	desc      *description.BuildDescription
	stageIdx  int
	platform  Platform
	ctx       *buildctx.Context
	cache     *cache.Cache
	bases     map[string]string
	opts      BuildOptions
	expander  *description.Expander
	store     *stageStore // completed stages (COPY --from)
	rootfs    string
	config    *oci.ImageConfig
	chainKey  digest.Digest
	dirs      map[string]bool // directories present in rootfs
	pending   []string        // dirs created by WORKDIR, folded into next layer
	layerSizes map[digest.Digest]int64
}

const scratchDigest = digest.Digest("sha256:0000000000000000000000000000000000000000000000000000000000000000")

// NewExecutor creates a stage executor.
func NewExecutor(desc *description.BuildDescription, stageIdx int, platform Platform,
	ctx *buildctx.Context, c *cache.Cache, bases map[string]string, opts BuildOptions,
	store *stageStore) *Executor {
	return &Executor{
		desc:       desc,
		stageIdx:   stageIdx,
		platform:   platform,
		ctx:        ctx,
		cache:      c,
		bases:      bases,
		opts:       opts,
		store:      store,
		dirs:       map[string]bool{},
		layerSizes: map[digest.Digest]int64{},
	}
}

// Run executes the stage and returns its result.
func (e *Executor) Run(rootfs string) (*StageResult, error) {
	e.rootfs = rootfs
	stage := &e.desc.Stages[e.stageIdx]

	// Initialise expander with global ARGs.
	e.expander = description.NewExpander(nil)
	for _, kv := range e.desc.GlobalARGs {
		for _, k := range kv.Order {
			e.expander.Set(k, kv.Pairs[k])
		}
	}

	// Prepare base.
	if stage.FromScratch {
		e.chainKey = scratchDigest
		e.config = oci.NewImageConfig(e.platform.OCI(), e.opts.Epoch)
	} else if src := e.stageBaseRef(stage); src != nil {
		if err := e.loadStageBase(src); err != nil {
			return nil, err
		}
	} else {
		base, err := e.loadBase(stage)
		if err != nil {
			return nil, err
		}
		if err := base.ExtractLayers(rootfs); err != nil {
			return nil, err
		}
		e.chainKey = base.ManifestDigest
		e.config = base.Config
		if e.config == nil {
			e.config = oci.NewImageConfig(e.platform.OCI(), e.opts.Epoch)
		}
		e.config.Created = e.opts.Epoch
		e.config.Platform = e.platform.OCI()
		e.config.OS = e.platform.OS
		e.config.Architecture = e.platform.Arch
		// record existing dirs from base
		if err := e.scanDirs(); err != nil {
			return nil, err
		}
	}

	for n, instr := range stage.Instructions {
		if err := e.exec(stage, n, instr); err != nil {
			return nil, err
		}
	}

	// Fold any pending WORKDIR directories into a final layer.
	if len(e.pending) > 0 {
		var files []oci.LayerFile
		for _, d := range e.pending {
			files = append(files, oci.LayerFile{RelPath: d, Type: oci.LayerDir})
		}
		if _, err := e.buildLayer("WORKDIR", files, nil); err != nil {
			return nil, err
		}
		e.pending = nil
	}

	// Build config blob.
	e.config.RootFS.Type = "layers"
	configData, configDigest, err := oci.MarshalJSONDeterministic(e.config)
	if err != nil {
		return nil, err
	}
	_ = configData

	// Build manifest.
	man := &oci.Manifest{
		SchemaVersion: 2,
		MediaType:     oci.MediaTypeManifest,
		Config: oci.Descriptor{
			MediaType: oci.MediaTypeConfig,
			Digest:    configDigest,
			Size:      int64(len(configData)),
		},
		Layers: e.layerDescriptors(),
	}
	manData, manDigest, err := oci.MarshalJSONDeterministic(man)
	if err != nil {
		return nil, err
	}
	_ = manData

	return &StageResult{
		StageName:      stage.Name,
		StageIndex:     stage.Index,
		Platform:       e.platform,
		Config:         e.config,
		Manifest:       man,
		ConfigDigest:   configDigest,
		ManifestDigest: manDigest,
		ChainKey:       e.chainKey,
		Rootfs:         rootfs,
	}, nil
}

func (e *Executor) loadBase(stage *description.Stage) (*BaseImage, error) {
	ref := stage.BaseImage
	// strip tag/digest for lookup
	name := ref
	if i := strings.IndexAny(ref, ":@"); i >= 0 {
		name = ref[:i]
	}
	tarPath, ok := e.bases[name]
	if !ok {
		return nil, description.NewStageError(stage.Name, stage.Index, 0, "FROM",
			"base image %q is not available (provide a local archive via --base %s=<path>)", ref, name)
	}
	bi, err := LoadBaseImage(name, tarPath, e.platform)
	if err != nil {
		return nil, description.NewStageError(stage.Name, stage.Index, 0, "FROM", "%s", err.Error())
	}
	return bi, nil
}

// stageBaseRef returns the source stage if FROM references another stage.
func (e *Executor) stageBaseRef(stage *description.Stage) *StageResult {
	ref := stage.BaseImage
	if i := strings.IndexAny(ref, ":@"); i >= 0 {
		ref = ref[:i]
	}
	// by name
	for j := range e.desc.Stages {
		if e.desc.Stages[j].Name == ref {
			return e.store.byName(ref, e.platform)
		}
	}
	// by index
	if idx, err := strconv.Atoi(ref); err == nil && idx >= 0 && idx < len(e.desc.Stages) {
		name := e.desc.Stages[idx].Name
		if name != "" {
			return e.store.byName(name, e.platform)
		}
		// unnamed stage: find by index via store
		return e.store.byIndex(idx, e.platform)
	}
	return nil
}

// loadStageBase inherits config and layers from a prior stage.
func (e *Executor) loadStageBase(src *StageResult) error {
	e.config = src.Config
	e.chainKey = src.ChainKey
	for _, d := range src.Manifest.Layers {
		e.layerSizes[d.Digest] = d.Size
		data, err := os.ReadFile(e.cache.BlobPath(d.Digest))
		if err != nil {
			return err
		}
		if err := extractTar(data, e.rootfs); err != nil {
			return err
		}
	}
	return e.scanDirs()
}

func (e *Executor) layerDescriptors() []oci.Descriptor {
	out := []oci.Descriptor{}
	for _, d := range e.config.RootFS.DiffIDs {
		out = append(out, oci.Descriptor{
			MediaType: oci.MediaTypeLayerTar,
			Digest:    d,
			Size:      e.layerSizes[d],
		})
	}
	return out
}

func (e *Executor) scanDirs() error {
	return filepath.Walk(e.rootfs, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(e.rootfs, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if fi.IsDir() {
			e.dirs[rel] = true
		}
		return nil
	})
}

func (e *Executor) exec(stage *description.Stage, n int, instr *description.Instruction) error {
	var err error
	switch instr.Kind {
	case description.InstCopy, description.InstAdd:
		err = e.handleCopy(stage, instr)
	case description.InstRun:
		err = e.handleRun(stage, instr)
	case description.InstEnv, description.InstLabel, description.InstArg:
		err = e.handleKV(stage, instr)
	case description.InstWorkdir:
		err = e.handleWorkdir(stage, instr)
	case description.InstUser:
		err = e.handleUser(stage, instr)
	case description.InstCmd, description.InstEntrypoint:
		err = e.handleCmd(stage, instr)
	case description.InstExpose:
		err = e.handleExpose(stage, instr)
	case description.InstVolume:
		err = e.handleVolume(stage, instr)
	case description.InstMaintainer:
		err = e.handleMaintainer(stage, instr)
	default:
		err = description.NewStageError(stage.Name, stage.Index, instr.Line, instr.Kind.String(),
			"unsupported instruction")
	}
	if err != nil {
		// Wrap with step location if not already locatable.
		if _, ok := err.(*description.StageError); ok {
			return err
		}
		return description.NewStageError(stage.Name, stage.Index, instr.Line, instr.Kind.String(), "%s", err.Error())
	}
	return nil
}

// ---- layer-producing instructions ----

func (e *Executor) handleCopy(stage *description.Stage, instr *description.Instruction) error {
	ca := instr.Args["copy"].(*description.CopyArgs)
	ca.Sources = e.expandAll(ca.Sources)
	ca.Dest = e.expander.Expand(ca.Dest)
	ca.From = e.expander.Expand(ca.From)
	ca.Chmod = e.expander.Expand(ca.Chmod)

	instKey := map[string]interface{}{
		"kind":    instr.Kind.String(),
		"sources": ca.Sources,
		"dest":    ca.Dest,
	}
	if ca.Chmod != "" {
		instKey["chmod"] = ca.Chmod
	}

	var files []oci.LayerFile
	if ca.From != "" {
		// COPY --from=<stage>
		src := e.store.byName(ca.From, e.platform)
		if src == nil {
			return description.NewStageError(stage.Name, stage.Index, instr.Line, "COPY",
				"--from references stage %q which does not exist", ca.From)
		}
		instKey["from"] = ca.From
		instKey["fromChain"] = src.ChainKey.String()
		entries, err := resolveStageSources(src.Rootfs, ca.Sources)
		if err != nil {
			return description.NewStageError(stage.Name, stage.Index, instr.Line, "COPY", "%s", err.Error())
		}
		files, err = e.mapStageEntries(entries, ca.Dest)
		if err != nil {
			return description.NewStageError(stage.Name, stage.Index, instr.Line, "COPY", "%s", err.Error())
		}
	} else {
		instKey["context"] = string(e.ctx.TreeDigest())
		entries, err := e.ctx.ResolveSources(ca.Sources)
		if err != nil {
			return description.NewStageError(stage.Name, stage.Index, instr.Line, "COPY", "%s", err.Error())
		}
		var err2 error
		files, err2 = e.mapContextEntries(entries, ca.Sources, ca.Dest, ca.Chmod)
		if err2 != nil {
			return description.NewStageError(stage.Name, stage.Index, instr.Line, "COPY", "%s", err2.Error())
		}
	}

	if instr.Kind == description.InstAdd {
		extracted, err := e.expandADDArchives(files, ca.Dest)
		if err != nil {
			return description.NewStageError(stage.Name, stage.Index, instr.Line, "ADD", "%s", err.Error())
		}
		files = extracted
	}

	if len(files) == 0 {
		return description.NewStageError(stage.Name, stage.Index, instr.Line, instr.Kind.String(),
			"no files to copy")
	}
	key, err := e.cacheKey(instKey)
	if err != nil {
		return err
	}
	if _, err := e.buildLayerWithKey(key, instr.Kind.String(), files, nil); err != nil {
		return description.NewStageError(stage.Name, stage.Index, instr.Line, instr.Kind.String(), "%s", err.Error())
	}
	e.markDirs(files)
	return nil
}

func (e *Executor) handleRun(stage *description.Stage, instr *description.Instruction) error {
	ra := instr.Args["run"].(*description.RunArgs)
	var args []string
	if ra.ExecForm {
		args = ra.ExecArgs
	} else {
		cmd := e.expander.Expand(ra.Command)
		args = []string{"/bin/sh", "-c", cmd}
	}
	instKey := map[string]interface{}{"kind": "RUN"}
	if ra.ExecForm {
		instKey["exec"] = args
	} else {
		instKey["shell"] = ra.Command
	}

	key, err := e.cacheKey(instKey)
	if err != nil {
		return err
	}
	if hit, err := e.cacheHit(key); err != nil {
		return err
	} else if hit {
		return nil
	}

	before, err := snapshotRootfs(e.rootfs)
	if err != nil {
		return err
	}
	env := e.buildEnv()
	if err := RunInRootfs(e.rootfs, args, env, e.config.Config.WorkingDir); err != nil {
		return description.NewStageError(stage.Name, stage.Index, instr.Line, "RUN", "%s", err.Error())
	}
	after, err := snapshotRootfs(e.rootfs)
	if err != nil {
		return err
	}
	files, deletions := diffRootfs(e.rootfs, before, after)
	if len(files) == 0 && len(deletions) == 0 {
		// RUN changed nothing; still advance the chain (empty layer history).
		e.chainKey = key
		e.config.History = append(e.config.History, oci.History{
			Created: e.opts.Epoch, CreatedBy: "RUN " + ra.Command, EmptyLayer: true,
		})
		return nil
	}
	if _, err := e.buildLayerWithKey(key, "RUN", files, deletions); err != nil {
		return description.NewStageError(stage.Name, stage.Index, instr.Line, "RUN", "%s", err.Error())
	}
	return nil
}

// ---- config-only instructions ----

func (e *Executor) handleKV(stage *description.Stage, instr *description.Instruction) error {
	kv := instr.Args["kv"].(*description.KeyValueArgs)
	instKey := map[string]interface{}{"kind": instr.Kind.String()}
	switch instr.Kind {
	case description.InstEnv:
		pairs := map[string]string{}
		for _, k := range kv.Order {
			v := e.expander.Expand(kv.Pairs[k])
			pairs[k] = v
			e.expander.Set(k, v)
			e.setEnv(k, v)
		}
		instKey["pairs"] = pairs
	case description.InstLabel:
		pairs := map[string]string{}
		for _, k := range kv.Order {
			v := e.expander.Expand(kv.Pairs[k])
			pairs[k] = v
			if e.config.Config.Labels == nil {
				e.config.Config.Labels = map[string]string{}
			}
			e.config.Config.Labels[k] = v
		}
		instKey["pairs"] = pairs
	case description.InstArg:
		pairs := map[string]string{}
		for _, k := range kv.Order {
			v := kv.Pairs[k]
			if v != "" {
				v = e.expander.Expand(v)
			}
			pairs[k] = v
			e.expander.Set(k, v)
		}
		instKey["pairs"] = pairs
	}
	e.advanceChain(instKey, instr.Kind.String()+" "+instr.Raw)
	return nil
}

func (e *Executor) handleWorkdir(stage *description.Stage, instr *description.Instruction) error {
	v := e.expander.Expand(instr.Args["value"].(string))
	if !path.IsAbs(v) {
		v = path.Join("/", e.config.Config.WorkingDir, v)
	}
	v = path.Clean(v)
	e.config.Config.WorkingDir = v
	// Create the directory in the rootfs.
	rel := strings.TrimPrefix(v, "/")
	if rel != "" && !e.dirs[rel] {
		if err := os.MkdirAll(filepath.Join(e.rootfs, rel), 0o755); err != nil {
			return err
		}
		e.dirs[rel] = true
		e.pending = append(e.pending, rel)
	}
	e.advanceChain(map[string]interface{}{"kind": "WORKDIR", "path": v}, "WORKDIR "+v)
	return nil
}

func (e *Executor) handleUser(stage *description.Stage, instr *description.Instruction) error {
	v := e.expander.Expand(instr.Args["value"].(string))
	e.config.Config.User = v
	e.advanceChain(map[string]interface{}{"kind": "USER", "user": v}, "USER "+v)
	return nil
}

func (e *Executor) handleCmd(stage *description.Stage, instr *description.Instruction) error {
	ra := instr.Args["run"].(*description.RunArgs)
	var args []string
	if ra.ExecForm {
		args = ra.ExecArgs
	} else {
		args = []string{"/bin/sh", "-c", e.expander.Expand(ra.Command)}
	}
	if instr.Kind == description.InstCmd {
		e.config.Config.Cmd = args
	} else {
		e.config.Config.Entrypoint = args
	}
	e.advanceChain(map[string]interface{}{"kind": instr.Kind.String(), "args": args}, instr.Kind.String()+" "+instr.Raw)
	return nil
}

func (e *Executor) handleExpose(stage *description.Stage, instr *description.Instruction) error {
	list := instr.Args["list"].([]string)
	ports := map[string]struct{}{}
	for _, p := range list {
		p = e.expander.Expand(p)
		if !strings.Contains(p, "/") {
			p = p + "/tcp"
		}
		ports[p] = struct{}{}
	}
	if e.config.Config.ExposedPorts == nil {
		e.config.Config.ExposedPorts = map[string]struct{}{}
	}
	for k := range ports {
		e.config.Config.ExposedPorts[k] = struct{}{}
	}
	e.advanceChain(map[string]interface{}{"kind": "EXPOSE", "ports": sortedKeys(ports)}, "EXPOSE "+strings.Join(sortedKeys(ports), " "))
	return nil
}

func (e *Executor) handleVolume(stage *description.Stage, instr *description.Instruction) error {
	list := instr.Args["list"].([]string)
	vols := map[string]struct{}{}
	for _, v := range list {
		v = e.expander.Expand(v)
		vols[v] = struct{}{}
		if err := os.MkdirAll(filepath.Join(e.rootfs, strings.TrimPrefix(v, "/")), 0o755); err != nil {
			return err
		}
	}
	if e.config.Config.Volumes == nil {
		e.config.Config.Volumes = map[string]struct{}{}
	}
	for k := range vols {
		e.config.Config.Volumes[k] = struct{}{}
	}
	e.advanceChain(map[string]interface{}{"kind": "VOLUME", "volumes": sortedKeys(vols)}, "VOLUME "+strings.Join(sortedKeys(vols), " "))
	return nil
}

func (e *Executor) handleMaintainer(stage *description.Stage, instr *description.Instruction) error {
	v := e.expander.Expand(instr.Args["value"].(string))
	e.config.Author = v
	e.advanceChain(map[string]interface{}{"kind": "MAINTAINER", "value": v}, "MAINTAINER "+v)
	return nil
}

// ---- helpers ----

func (e *Executor) advanceChain(instKey map[string]interface{}, createdBy string) error {
	key, err := e.cacheKey(instKey)
	if err != nil {
		return err
	}
	e.chainKey = key
	e.config.History = append(e.config.History, oci.History{
		Created: e.opts.Epoch, CreatedBy: createdBy, EmptyLayer: true,
	})
	return nil
}

func (e *Executor) cacheKey(instKey map[string]interface{}) (digest.Digest, error) {
	m := map[string]interface{}{
		"v":        1,
		"parent":   e.chainKey.String(),
		"platform": e.platform.String(),
		"inst":     instKey,
	}
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return digest.FromBytes(data), nil
}

func (e *Executor) cacheHit(key digest.Digest) (bool, error) {
	entry, hit, err := e.cache.Get(key)
	if err != nil {
		return false, err
	}
	if !hit {
		return false, nil
	}
	if entry.Meta.LayerDigest != "" {
		blobPath := e.cache.BlobPath(entry.Meta.LayerDigest)
		data, err := os.ReadFile(blobPath)
		if err != nil {
			return false, err
		}
		if err := extractTar(data, e.rootfs); err != nil {
			return false, err
		}
		e.config.RootFS.DiffIDs = append(e.config.RootFS.DiffIDs, entry.Meta.LayerDigest)
		e.layerSizes[entry.Meta.LayerDigest] = entry.Meta.Size
		e.config.History = append(e.config.History, oci.History{
			Created: e.opts.Epoch, CreatedBy: entry.Meta.Note,
		})
	}
	return true, nil
}

// buildLayer builds a layer from files/deletions, checks the size limit,
// installs it into the cache and extracts it into the rootfs.
func (e *Executor) buildLayer(note string, files []oci.LayerFile, deletions []string) (digest.Digest, error) {
	key, err := e.cacheKey(map[string]interface{}{"kind": note})
	if err != nil {
		return "", err
	}
	return e.buildLayerWithKey(key, note, files, deletions)
}

func (e *Executor) buildLayerWithKey(key digest.Digest, note string, files []oci.LayerFile, deletions []string) (digest.Digest, error) {
	// Cache hit?
	if hit, err := e.cacheHit(key); err != nil {
		return "", err
	} else if hit {
		return key, nil
	}

	tmp, err := os.CreateTemp(e.cache.TmpDir(), "layer-*.tar")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	d, n, err := oci.WriteLayerTar(tmp, e.opts.Epoch, files, deletions)
	if err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if e.opts.MaxLayerSize > 0 && n > e.opts.MaxLayerSize {
		return "", fmt.Errorf("layer content is too large: %d bytes (limit %d)", n, e.opts.MaxLayerSize)
	}
	// Extract into rootfs before the cache takes ownership of the file.
	data, err := os.ReadFile(tmpName)
	if err != nil {
		return "", err
	}
	if err := extractTar(data, e.rootfs); err != nil {
		return "", err
	}
	if err := e.cache.Put(key, tmpName, cache.Meta{
		LayerDigest: d, Size: n, Epoch: e.opts.Epoch.Unix(), Note: note,
	}); err != nil {
		return "", err
	}
	e.chainKey = key
	e.config.RootFS.DiffIDs = append(e.config.RootFS.DiffIDs, d)
	e.layerSizes[d] = n
	e.config.History = append(e.config.History, oci.History{
		Created: e.opts.Epoch, CreatedBy: note,
	})
	return key, nil
}

func (e *Executor) expandAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = e.expander.Expand(s)
	}
	return out
}

func (e *Executor) setEnv(k, v string) {
	prefix := k + "="
	found := false
	for i, ev := range e.config.Config.Env {
		if strings.HasPrefix(ev, prefix) {
			e.config.Config.Env[i] = prefix + v
			found = true
			break
		}
	}
	if !found {
		e.config.Config.Env = append(e.config.Config.Env, prefix+v)
	}
}

func (e *Executor) buildEnv() []string {
	env := append([]string{}, e.config.Config.Env...)
	env = append(env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	return env
}

func (e *Executor) markDirs(files []oci.LayerFile) {
	for _, f := range files {
		rel := path.Clean(f.RelPath)
		parent := path.Dir(rel)
		for parent != "." && parent != "/" {
			e.dirs[parent] = true
			parent = path.Dir(parent)
		}
		if f.Type == oci.LayerDir {
			e.dirs[rel] = true
		}
	}
}

// mapContextEntries maps resolved context entries to destination paths.
func (e *Executor) mapContextEntries(entries []buildctx.Entry, sources []string, dest, chmod string) ([]oci.LayerFile, error) {
	dest = e.resolveDest(dest)
	destIsDir := strings.HasSuffix(dest, "/") || len(sources) > 1 || e.dirs[strings.TrimPrefix(path.Clean(dest), "/")]
	var files []oci.LayerFile
	mode := os.FileMode(0)
	if chmod != "" {
		m, err := parseChmod(chmod)
		if err != nil {
			return nil, err
		}
		mode = m
	}
	for _, en := range entries {
		target := dest
		if destIsDir {
			target = path.Join(dest, path.Base(en.RelPath))
		} else if len(sources) > 1 {
			target = path.Join(dest, path.Base(en.RelPath))
		}
		lf := oci.LayerFile{
			RelPath: strings.TrimPrefix(path.Clean(target), "/"),
			AbsPath: en.AbsPath,
			Mode:    mode,
		}
		switch en.Type {
		case buildctx.EntrySymlink:
			lf.Type = oci.LayerSymlink
			lf.Target = en.Target
		case buildctx.EntryDir:
			lf.Type = oci.LayerDir
		default:
			lf.Type = oci.LayerRegular
		}
		files = append(files, lf)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].RelPath < files[j].RelPath })
	return files, nil
}

// mapStageEntries maps files from a COPY --from stage rootfs to destinations.
func (e *Executor) mapStageEntries(entries []stageEntry, dest string) ([]oci.LayerFile, error) {
	dest = e.resolveDest(dest)
	destIsDir := strings.HasSuffix(dest, "/") || len(entries) > 1
	var files []oci.LayerFile
	for _, en := range entries {
		target := dest
		if destIsDir {
			target = path.Join(dest, path.Base(en.rel))
		}
		lf := oci.LayerFile{
			RelPath: strings.TrimPrefix(path.Clean(target), "/"),
			AbsPath: en.absPath,
		}
		switch en.typ {
		case oci.LayerSymlink:
			lf.Type = oci.LayerSymlink
			lf.Target = en.target
		case oci.LayerDir:
			lf.Type = oci.LayerDir
		default:
			lf.Type = oci.LayerRegular
		}
		files = append(files, lf)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].RelPath < files[j].RelPath })
	return files, nil
}

func (e *Executor) resolveDest(dest string) string {
	if !path.IsAbs(dest) {
		dest = path.Join("/", e.config.Config.WorkingDir, dest)
	}
	return path.Clean(dest)
}

type stageEntry struct {
	rel     string
	absPath string
	typ     oci.LayerFileType
	target  string
}

// resolveStageSources walks a source stage rootfs and matches source patterns.
func resolveStageSources(rootfs string, patterns []string) ([]stageEntry, error) {
	var all []stageEntry
	err := filepath.Walk(rootfs, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(rootfs, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		se := stageEntry{rel: rel, absPath: p}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			se.typ = oci.LayerSymlink
			tgt, _ := os.Readlink(p)
			se.target = tgt
		case fi.IsDir():
			se.typ = oci.LayerDir
		default:
			se.typ = oci.LayerRegular
		}
		all = append(all, se)
		return nil
	})
	if err != nil {
		return nil, err
	}
	var out []stageEntry
	seen := map[string]bool{}
	for _, pat := range patterns {
		pat = filepath.ToSlash(pat)
		pat = strings.TrimPrefix(pat, "/")
		matched := false
		for _, se := range all {
			ok, merr := filepath.Match(pat, se.rel)
			if merr != nil {
				return nil, fmt.Errorf("invalid source pattern %q: %w", pat, merr)
			}
			if !ok {
				continue
			}
			matched = true
			if se.typ == oci.LayerDir {
				// include directory contents recursively
				prefix := se.rel + "/"
				for _, sub := range all {
					if strings.HasPrefix(sub.rel, prefix) && !seen[sub.rel] {
						seen[sub.rel] = true
						out = append(out, sub)
					}
				}
				if !seen[se.rel] {
					seen[se.rel] = true
					out = append(out, se)
				}
			} else if !seen[se.rel] {
				seen[se.rel] = true
				out = append(out, se)
			}
		}
		if !matched {
			return nil, fmt.Errorf("no files matching %q in source stage", pat)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out, nil
}

// expandADDArchives extracts local tar archives referenced by ADD.
func (e *Executor) expandADDArchives(files []oci.LayerFile, dest string) ([]oci.LayerFile, error) {
	var out []oci.LayerFile
	for _, f := range files {
		if f.Type != oci.LayerRegular || !isArchive(f.RelPath) {
			out = append(out, f)
			continue
		}
		// Extract archive into a temp dir, then include its contents.
		tmp, err := os.MkdirTemp(e.cache.TmpDir(), "add-extract-*")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(tmp)
		data, err := os.ReadFile(f.AbsPath)
		if err != nil {
			return nil, err
		}
		if err := extractTar(data, tmp); err != nil {
			return nil, fmt.Errorf("extract archive %q: %w", path.Base(f.RelPath), err)
		}
		base := strings.TrimPrefix(path.Clean(dest), "/")
		if strings.HasSuffix(dest, "/") {
			base = path.Join(base, strings.TrimSuffix(path.Base(f.RelPath), path.Ext(f.RelPath)))
		}
		err = filepath.Walk(tmp, func(p string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			rel, rerr := filepath.Rel(tmp, p)
			if rerr != nil {
				return rerr
			}
			rel = filepath.ToSlash(rel)
			if rel == "." {
				return nil
			}
			target := path.Join(base, rel)
			lf := oci.LayerFile{
				RelPath: strings.TrimPrefix(path.Clean(target), "/"),
				AbsPath: p,
			}
			switch {
			case fi.Mode()&os.ModeSymlink != 0:
				lf.Type = oci.LayerSymlink
				lf.Target, _ = os.Readlink(p)
			case fi.IsDir():
				lf.Type = oci.LayerDir
			default:
				lf.Type = oci.LayerRegular
			}
			out = append(out, lf)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelPath < out[j].RelPath })
	return out, nil
}

func isArchive(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".tar") || strings.HasSuffix(lower, ".tar.gz") ||
		strings.HasSuffix(lower, ".tgz") || strings.HasSuffix(lower, ".tar.bz2")
}

func parseChmod(s string) (os.FileMode, error) {
	var m os.FileMode
	for _, c := range s {
		if c < '0' || c > '7' {
			return 0, fmt.Errorf("invalid --chmod value %q: expected octal mode", s)
		}
		m = m*8 + os.FileMode(c-'0')
	}
	return m, nil
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- rootfs snapshot/diff for RUN ----

type fileState struct {
	typ    oci.LayerFileType
	hash   digest.Digest
	target string
}

func snapshotRootfs(rootfs string) (map[string]fileState, error) {
	out := map[string]fileState{}
	err := filepath.Walk(rootfs, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(rootfs, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		st := fileState{}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			st.typ = oci.LayerSymlink
			st.target, _ = os.Readlink(p)
		case fi.IsDir():
			st.typ = oci.LayerDir
		default:
			st.typ = oci.LayerRegular
			d, err := digest.FromFile(p)
			if err != nil {
				return err
			}
			st.hash = d
		}
		out[rel] = st
		return nil
	})
	return out, err
}

func diffRootfs(rootfs string, before, after map[string]fileState) (files []oci.LayerFile, deletions []string) {
	for rel, st := range after {
		old, ok := before[rel]
		if !ok {
			if st.typ == oci.LayerDir {
				continue // new dirs are implicit via parents
			}
			files = append(files, oci.LayerFile{
				RelPath: rel, AbsPath: filepath.Join(rootfs, rel), Type: st.typ, Target: st.target,
			})
			continue
		}
		if old.typ != st.typ || old.hash != st.hash || old.target != st.target {
			if st.typ == oci.LayerDir {
				continue
			}
			files = append(files, oci.LayerFile{
				RelPath: rel, AbsPath: filepath.Join(rootfs, rel), Type: st.typ, Target: st.target,
			})
		}
	}
	for rel := range before {
		if _, ok := after[rel]; !ok {
			deletions = append(deletions, rel)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].RelPath < files[j].RelPath })
	sort.Strings(deletions)
	return files, deletions
}
