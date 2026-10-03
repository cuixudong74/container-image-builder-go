package engine

import (
	"sort"
	"strings"
)

// vfsNode is one file in a stage's virtual filesystem. Regular file
// content is always read from Src, a host filesystem path (a context file
// or an extracted base-image file); layers are only packaging.
type vfsNode struct {
	Mode   int64  // permission bits
	IsDir  bool
	Link   string // symlink target
	Size   int64
	Digest string // content digest for regular files
	Src    string // host path backing a regular file
}

// vfs is the virtual filesystem of one stage on one platform.
type vfs struct {
	nodes map[string]*vfsNode // clean slash path, no leading slash
}

func newVFS() *vfs { return &vfs{nodes: map[string]*vfsNode{}} }

// cleanPath normalises an image-internal path: strips leading slashes,
// resolves "." and duplicate separators. Returns "" for the root.
func cleanPath(p string) string {
	p = strings.TrimSpace(p)
	parts := strings.Split(p, "/")
	var out []string
	for _, s := range parts {
		switch s {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, s)
		}
	}
	return strings.Join(out, "/")
}

func (v *vfs) set(path string, n *vfsNode) {
	v.nodes[cleanPath(path)] = n
}

func (v *vfs) get(path string) (*vfsNode, bool) {
	n, ok := v.nodes[cleanPath(path)]
	return n, ok
}

// ensureParents creates missing ancestor directories (mode 0755) for path.
func (v *vfs) ensureParents(path string) {
	p := cleanPath(path)
	for i := 1; i < len(p); i++ {
		if p[i] == '/' {
			dir := p[:i]
			if _, ok := v.nodes[dir]; !ok {
				v.nodes[dir] = &vfsNode{Mode: 0o755, IsDir: true}
			}
		}
	}
}

// subtree returns all nodes at or below path, sorted by path. The returned
// slice includes the root node itself when present.
func (v *vfs) subtree(path string) []vfsEntry {
	p := cleanPath(path)
	var out []vfsEntry
	for k, n := range v.nodes {
		if k == p || strings.HasPrefix(k, p+"/") {
			out = append(out, vfsEntry{Path: k, Node: n})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

type vfsEntry struct {
	Path string
	Node *vfsNode
}
