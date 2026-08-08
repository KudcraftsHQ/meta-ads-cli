// Package tree exposes the generated Marketing API command tree.
//
// The tree is produced by tools/gen_command_tree.py from Meta's own Python SDK
// and embedded at build time. It is split so that a normal invocation reads as
// little as possible: meta.json is 130 bytes, resolving a resource is a lookup
// in the embedded filesystem followed by unmarshalling that one file, and the
// 200 KB index is only touched by discovery commands and shell completion.
package tree

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
)

//go:embed all:schemas
var schemas embed.FS

// Kinds a parameter value can take. The dispatch layer coerces flag strings
// into the JSON shape the Graph API expects based on these.
const (
	KindString   = "string"
	KindInt      = "int"
	KindFloat    = "float"
	KindBool     = "bool"
	KindJSON     = "json"
	KindList     = "list"
	KindEnum     = "enum"
	KindFile     = "file"
	KindDatetime = "datetime"
)

// API types as the SDK labels them: a NODE op addresses the object itself, an
// EDGE op addresses a collection hanging off it.
const (
	TypeNode = "NODE"
	TypeEdge = "EDGE"
)

// Meta describes the API version the tree was generated against.
type Meta struct {
	Version       int    `json:"version"`
	APIVersion    string `json:"api_version"`
	GraphURL      string `json:"graph_url"`
	GraphVideoURL string `json:"graph_video_url"`
}

// Param is one accepted request parameter.
type Param struct {
	Name string   `json:"name"`
	Flag string   `json:"flag"`
	Type string   `json:"type"` // the SDK's own type string, shown in describe
	Kind string   `json:"kind"`
	Item string   `json:"item,omitempty"` // element kind when Kind == KindList
	Enum []string `json:"enum,omitempty"`
}

// Op is a single callable operation on a resource.
type Op struct {
	Name            string  `json:"name"`
	Method          string  `json:"method"`
	Endpoint        string  `json:"endpoint"`
	APIType         string  `json:"api_type"`
	AllowFileUpload bool    `json:"allow_file_upload"`
	Params          []Param `json:"params"`
}

// Param returns the named parameter, matching on either the API name or the
// kebab-case flag.
func (o *Op) Param(name string) (*Param, bool) {
	for i := range o.Params {
		if o.Params[i].Name == name || o.Params[i].Flag == name {
			return &o.Params[i], true
		}
	}
	return nil, false
}

// Resource is a Graph API node type and everything callable on it.
type Resource struct {
	Name   string   `json:"name"`
	Class  string   `json:"class"`
	Fields []string `json:"fields"`
	Ops    []Op     `json:"ops"`
}

// Op returns the named operation.
func (r *Resource) Op(name string) (*Op, bool) {
	for i := range r.Ops {
		if r.Ops[i].Name == name {
			return &r.Ops[i], true
		}
	}
	return nil, false
}

// IndexOp is the summary form carried in index.json.
type IndexOp struct {
	Name            string `json:"name"`
	Method          string `json:"method"`
	Endpoint        string `json:"endpoint"`
	APIType         string `json:"api_type"`
	Params          int    `json:"params"`
	AllowFileUpload bool   `json:"allow_file_upload"`
}

// IndexResource is the summary form of a resource.
type IndexResource struct {
	Name  string    `json:"name"`
	Class string    `json:"class"`
	Ops   []IndexOp `json:"ops"`
}

// Index is the full catalog without parameter detail.
type Index struct {
	Resources []IndexResource `json:"resources"`
}

// Resource returns the summary entry for a resource name.
func (i *Index) Resource(name string) (*IndexResource, bool) {
	for k := range i.Resources {
		if i.Resources[k].Name == name {
			return &i.Resources[k], true
		}
	}
	return nil, false
}

var (
	metaOnce  sync.Once
	metaValue Meta
	metaErr   error

	indexOnce  sync.Once
	indexValue Index
	indexErr   error

	namesOnce  sync.Once
	namesValue []string
	namesErr   error

	resourceMu    sync.Mutex
	resourceCache = map[string]*Resource{}
)

// LoadMeta returns the API version and Graph URLs. Cheap enough to call freely.
func LoadMeta() (Meta, error) {
	metaOnce.Do(func() {
		raw, err := schemas.ReadFile("schemas/meta.json")
		if err != nil {
			metaErr = fmt.Errorf("read embedded meta.json: %w", err)
			return
		}
		metaErr = json.Unmarshal(raw, &metaValue)
	})
	return metaValue, metaErr
}

// LoadIndex parses the full catalog. Only discovery commands and completion
// need this; ordinary dispatch goes through LoadResource.
func LoadIndex() (*Index, error) {
	indexOnce.Do(func() {
		raw, err := schemas.ReadFile("schemas/index.json")
		if err != nil {
			indexErr = fmt.Errorf("read embedded index.json: %w", err)
			return
		}
		indexErr = json.Unmarshal(raw, &indexValue)
	})
	if indexErr != nil {
		return nil, indexErr
	}
	return &indexValue, nil
}

// ResourceNames lists every resource without parsing any JSON at all -- it is a
// directory listing of the embedded filesystem.
func ResourceNames() ([]string, error) {
	namesOnce.Do(func() {
		entries, err := fs.ReadDir(schemas, "schemas/resources")
		if err != nil {
			namesErr = fmt.Errorf("list embedded resources: %w", err)
			return
		}
		namesValue = make([]string, 0, len(entries))
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			namesValue = append(namesValue, strings.TrimSuffix(e.Name(), ".json"))
		}
		sort.Strings(namesValue)
	})
	if namesErr != nil {
		return nil, namesErr
	}
	return namesValue, nil
}

// HasResource reports whether a resource exists, without unmarshalling it.
func HasResource(name string) bool {
	_, err := fs.Stat(schemas, "schemas/resources/"+name+".json")
	return err == nil
}

// LoadResource unmarshals exactly one resource file.
func LoadResource(name string) (*Resource, error) {
	resourceMu.Lock()
	defer resourceMu.Unlock()
	if r, ok := resourceCache[name]; ok {
		return r, nil
	}
	raw, err := schemas.ReadFile("schemas/resources/" + name + ".json")
	if err != nil {
		return nil, fmt.Errorf("unknown resource %q", name)
	}
	var r Resource
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("parse resource %q: %w", name, err)
	}
	resourceCache[name] = &r
	return &r, nil
}

// Suggest returns resource names close to a mistyped one, for error messages.
//
// Prefixes and substrings come first because they are usually an abbreviation
// rather than a mistake. Edit distance catches the actual typos -- a dropped
// letter in "ad-acount" shares no useful substring with "ad-account".
func Suggest(name string, limit int) []string {
	names, err := ResourceNames()
	if err != nil {
		return nil
	}
	lower := strings.ToLower(name)

	var prefix, substring []string
	type scored struct {
		name string
		dist int
	}
	var near []scored

	// One edit per four characters, so short names are not matched to anything.
	budget := len(lower)/4 + 1

	for _, n := range names {
		switch {
		case strings.HasPrefix(n, lower):
			prefix = append(prefix, n)
		case strings.Contains(n, lower):
			substring = append(substring, n)
		default:
			if d := editDistance(lower, n, budget); d <= budget {
				near = append(near, scored{n, d})
			}
		}
	}
	sort.SliceStable(near, func(i, j int) bool { return near[i].dist < near[j].dist })

	out := append(prefix, substring...)
	for _, s := range near {
		out = append(out, s.name)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// editDistance is Levenshtein, abandoned early once every cell in a row exceeds
// the budget -- with 309 candidates the cutoff matters more than the exact
// distance of the ones that were never going to match.
func editDistance(a, b string, budget int) int {
	if abs(len(a)-len(b)) > budget {
		return budget + 1
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		best := curr[0]
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
			if curr[j] < best {
				best = curr[j]
			}
		}
		if best > budget {
			return budget + 1
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
