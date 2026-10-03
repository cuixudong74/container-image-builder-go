package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/cuixudong74/container-image-builder-go/pkg/archive"
	"github.com/cuixudong74/container-image-builder-go/pkg/buildctx"
	"github.com/cuixudong74/container-image-builder-go/pkg/buildfile"
)

// copyPlan is the resolved set of tar entries a COPY instruction creates.
type copyPlan struct {
	entries []plannedEntry
}

type plannedEntry struct {
	name     string // path inside the layer tar
	mode     int64
	isDir    bool
	link     string
	size     int64
	digest   string // content digest (regular files)
	src      string // host path to read content from (regular files)
}

// planCopy resolves a COPY instruction against either the build context or
// a source stage's vfs and returns the deterministic layer entries plus the
// nodes to merge into the target vfs.
func planCopy(ins *buildfile.Instruction, workdir string, bctx *buildctx.Context, srcStage *stageState) (*copyPlan, error) {
	if len(ins.CopySrcs) == 0 {
		return nil, fmt.Errorf("COPY requires at least one source")
	}
	dest := ins.CopyDest
	destIsDir := strings.HasSuffix(dest, "/") || dest == "." || dest == ""
	if len(ins.CopySrcs) > 1 && !destIsDir {
		return nil, fmt.Errorf("COPY with multiple sources requires a destination ending in /")
	}
	destBase := cleanPath(joinWorkdir(workdir, dest))

	chmod := int64(-1)
	if ins.CopyChmod != "" {
		v, err := strconv.ParseUint(ins.CopyChmod, 8, 32)
		if err != nil || v > 0o7777 {
			return nil, fmt.Errorf("COPY: invalid --chmod %q", ins.CopyChmod)
		}
		chmod = int64(v)
	}

	p := &copyPlan{}
	for _, src := range ins.CopySrcs {
		var files []vfsEntry
		var err error
		if ins.CopyFrom == "" {
			files, err = scanContextSource(bctx, src)
		} else {
			files, err = scanStageSource(srcStage, src)
		}
		if err != nil {
			return nil, fmt.Errorf("COPY source %s: %w", src, err)
		}
		if len(files) == 0 {
			return nil, fmt.Errorf("COPY source %s matched no files", src)
		}
		// files[0] is the source root itself.
		root := files[0]
		srcBase := baseName(src)
		for _, f := range files {
			var name string
			switch {
			case f.Path == root.Path && !root.Node.IsDir:
				// Single file: dest is the file path, or dest/srcBase if dest is a dir.
				if destIsDir {
					name = joinLayer(destBase, srcBase)
				} else {
					name = destBase
				}
			case f.Path == root.Path && root.Node.IsDir:
				name = destBase
			default:
				rel := strings.TrimPrefix(f.Path, root.Path+"/")
				name = joinLayer(destBase, rel)
			}
			pe := plannedEntry{
				name:   name,
				mode:   f.Node.Mode,
				isDir:  f.Node.IsDir,
				link:   f.Node.Link,
				size:   f.Node.Size,
				digest: f.Node.Digest,
				src:    f.Node.Src,
			}
			if chmod >= 0 && !pe.isDir {
				pe.mode = chmod
			}
			p.entries = append(p.entries, pe)
		}
	}
	// Deterministic order; de-duplicate identical target paths (later
	// sources win, matching overlay semantics).
	byName := map[string]plannedEntry{}
	for _, e := range p.entries {
		byName[e.name] = e
	}
	p.entries = p.entries[:0]
	for _, e := range byName {
		p.entries = append(p.entries, e)
	}
	sort.Slice(p.entries, func(i, j int) bool { return p.entries[i].name < p.entries[j].name })
	return p, nil
}

// scanContextSource resolves src inside the build context into vfsEntries.
func scanContextSource(bctx *buildctx.Context, src string) ([]vfsEntry, error) {
	rel := cleanPath(src)
	if rel == "" {
		return nil, fmt.Errorf("cannot copy context root; list explicit paths")
	}
	info, err := bctx.Stat(rel)
	if err != nil {
		return nil, err
	}
	nodeFromEntry := func(e buildctx.FileEntry) *vfsNode {
		n := &vfsNode{
			Mode:   archive.NormalizeMode(e.Mode),
			IsDir:  e.IsDir,
			Link:   e.Linkname,
			Size:   e.Size,
			Digest: e.Digest,
		}
		if !e.IsDir && e.Linkname == "" {
			n.Src = bctx.AbsPath(e.Path)
		}
		return n
	}
	if !info.IsDir {
		return []vfsEntry{{Path: rel, Node: nodeFromEntry(info)}}, nil
	}
	// Directory: recursive scan, filtered by ignore rules.
	all, err := bctx.Scan()
	if err != nil {
		return nil, err
	}
	var out []vfsEntry
	out = append(out, vfsEntry{Path: rel, Node: nodeFromEntry(info)})
	prefix := rel + "/"
	for _, e := range all {
		if strings.HasPrefix(e.Path, prefix) {
			out = append(out, vfsEntry{Path: e.Path, Node: nodeFromEntry(e)})
		}
	}
	return out, nil
}

// scanStageSource resolves src inside a built stage's vfs.
func scanStageSource(st *stageState, src string) ([]vfsEntry, error) {
	rel := cleanPath(src)
	if rel == "" {
		return nil, fmt.Errorf("cannot copy stage root; list explicit paths")
	}
	if _, ok := st.fs.get(rel); !ok {
		return nil, fmt.Errorf("path /%s not found in stage %s", rel, st.name)
	}
	return st.fs.subtree(rel), nil
}

// inputDigest computes the content hash of a copy plan: the cache key
// ingredient that makes layer identity depend purely on input content.
func (p *copyPlan) inputDigest() string {
	h := sha256.New()
	for _, e := range p.entries {
		fmt.Fprintf(h, "%s\x00%o\x00", e.name, e.mode)
		switch {
		case e.isDir:
			h.Write([]byte("dir\x00"))
		case e.link != "":
			fmt.Fprintf(h, "symlink\x00%s\x00", e.link)
		default:
			fmt.Fprintf(h, "file\x00%s\x00", e.digest)
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// toArchiveEntries converts the plan into archive entries with implicit
// parent directories (mode 0755) prepended, all sorted by name. A real
// directory entry always replaces its implicit placeholder.
func (p *copyPlan) toArchiveEntries() []archive.Entry {
	byName := map[string]archive.Entry{}
	for _, e := range p.entries {
		for i := 1; i < len(e.name); i++ {
			if e.name[i] == '/' {
				d := e.name[:i]
				if _, ok := byName[d]; !ok {
					byName[d] = archive.Entry{Name: d, Typeflag: tarTypeDir, Mode: 0o755}
				}
			}
		}
		ae := archive.Entry{Name: e.name, Mode: e.mode, Size: e.size, Linkname: e.link}
		switch {
		case e.isDir:
			ae.Typeflag = tarTypeDir
		case e.link != "":
			ae.Typeflag = tarTypeSymlink
		}
		byName[e.name] = ae
	}
	out := make([]archive.Entry, 0, len(byName))
	for _, e := range byName {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// openPlanned returns an OpenFunc resolving regular-file entries to their
// backing host files.
func openPlanned(byName map[string]plannedEntry) archive.OpenFunc {
	return func(e archive.Entry) (io.ReadCloser, error) {
		pe, ok := byName[e.Name]
		if !ok {
			return nil, fmt.Errorf("archive: no source for %s", e.Name)
		}
		return os.Open(pe.src)
	}
}

// plannedByName indexes the plan entries by layer path.
func (p *copyPlan) plannedByName() map[string]plannedEntry {
	m := make(map[string]plannedEntry, len(p.entries))
	for _, e := range p.entries {
		m[e.name] = e
	}
	return m
}

func joinWorkdir(workdir, dest string) string {
	if strings.HasPrefix(dest, "/") {
		return dest
	}
	if workdir == "" || workdir == "/" {
		return "/" + dest
	}
	return strings.TrimSuffix(workdir, "/") + "/" + dest
}

func joinLayer(base, rel string) string {
	if base == "" {
		return rel
	}
	return base + "/" + rel
}

func baseName(p string) string {
	p = strings.TrimSuffix(cleanPath(p), "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

const (
	tarTypeDir     = '5'
	tarTypeSymlink = '2'
)
