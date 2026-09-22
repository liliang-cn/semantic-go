package semantic

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Domains
//
// A client does not have "a semantic model". It has a handful of them — shifts,
// billing, rostering — each a governed vocabulary over part of the warehouse,
// each published separately and owned by different people.
//
// Routing to the right one has to happen before anything else, and it is the
// one step that should cost no round trip: the whole list is a line per domain,
// small enough to sit in an agent's context permanently. Everything after it —
// which metrics, which dimensions, is this join safe — is a question about ONE
// domain, and asking it of the wrong domain wastes a turn at best.
// ---------------------------------------------------------------------------

// Domain is one published semantic model under a name.
type Domain struct {
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	Instructions string `json:"instructions,omitempty"` // ai_context.instructions, verbatim
	Model        *Model `json:"-"`
}

// DomainInfo is one line of the routing catalog: enough to choose, and nothing
// more. Metric names are deliberately absent — a client with fifteen domains of
// forty metrics would spend the whole budget on a list that answers a question
// nobody asked yet. list_metrics on the chosen domain is the next step.
type DomainInfo struct {
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	Instructions string `json:"instructions,omitempty"`
	Metrics      int    `json:"metric_count"`
	Datasets     int    `json:"dataset_count"`
}

// Catalog is the set of domains one server publishes.
type Catalog struct {
	domains []Domain
	byName  map[string]*Domain
}

// NewCatalog indexes domains by name, refusing duplicates: two domains sharing
// a name means one of them can never be routed to, and which one is an accident
// of ordering.
func NewCatalog(domains ...Domain) (*Catalog, error) {
	c := &Catalog{byName: map[string]*Domain{}}
	// Duplicates are detected against a plain set, not against byName: byName
	// holds pointers INTO c.domains, which is still growing here, so it can
	// only be built once the slice has stopped moving. Checking against it
	// inside this loop looked right and silently never fired.
	seen := map[string]bool{}
	for _, d := range domains {
		if d.Name == "" {
			return nil, fmt.Errorf("a domain has no name")
		}
		if d.Model == nil {
			return nil, fmt.Errorf("domain %q has no model", d.Name)
		}
		if seen[d.Name] {
			return nil, fmt.Errorf("two domains are both named %q: one of them could never be routed to, "+
				"and which one is an accident of load order", d.Name)
		}
		seen[d.Name] = true
		c.domains = append(c.domains, d)
	}
	for i := range c.domains {
		c.byName[c.domains[i].Name] = &c.domains[i]
	}
	return c, nil
}

// LoadCatalog reads a catalog from a path.
//
// A FILE is a single native model, named after its base name — or an
// interchange document, which may itself carry several domains. A DIRECTORY is
// one domain per *.yaml / *.yml / *.json inside it, sorted by name so the
// routing list is stable between runs: an agent that keeps the catalog in its
// context should not see it reshuffle because a filesystem felt like it.
func LoadCatalog(path string) (*Catalog, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		domains, err := loadDomainFile(path)
		if err != nil {
			return nil, err
		}
		return NewCatalog(domains...)
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".yaml", ".yml", ".json":
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("no .yaml, .yml or .json model files in %s", path)
	}
	var all []Domain
	for _, n := range names {
		domains, err := loadDomainFile(filepath.Join(path, n))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		all = append(all, domains...)
	}
	return NewCatalog(all...)
}

// loadDomainFile reads one file as either a native model or an interchange
// document. Which it is decided by content, not by extension: a dbt build
// writes JSON, a hand-authored model is usually YAML, and both spellings of
// both formats turn up in the wild.
func loadDomainFile(path string) ([]Domain, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))

	if looksLikeInterchange(data) {
		return ImportOssie(data)
	}
	m, err := Load(data)
	if err != nil {
		return nil, err
	}
	// A model that names itself wins over the file it arrived in: the file can
	// be renamed, copied or served from a directory somebody else lays out, and
	// the domain an agent routed to yesterday should still be there today.
	if m.Name != "" {
		name = m.Name
	}
	return []Domain{{
		Name:         name,
		Description:  m.Description,
		Instructions: m.Instructions,
		Model:        m,
	}}, nil
}

func looksLikeInterchange(data []byte) bool {
	return strings.Contains(string(data), "semantic_model")
}

// Domains returns the routing catalog, in load order.
func (c *Catalog) Domains() []DomainInfo {
	out := make([]DomainInfo, 0, len(c.domains))
	for i := range c.domains {
		d := &c.domains[i]
		out = append(out, DomainInfo{
			Name:         d.Name,
			Description:  d.Description,
			Instructions: d.Instructions,
			Metrics:      len(d.Model.Metrics),
			Datasets:     len(d.Model.Entities),
		})
	}
	return out
}

// Names returns the domain names in load order.
func (c *Catalog) Names() []string {
	out := make([]string, len(c.domains))
	for i := range c.domains {
		out[i] = c.domains[i].Name
	}
	return out
}

func (c *Catalog) Len() int { return len(c.domains) }

// Get resolves a domain by name. An empty name is allowed only when there is
// exactly one domain: guessing which of several a caller meant is how a
// question about billing gets answered out of the rostering model, with a
// number that is real and about the wrong thing.
func (c *Catalog) Get(name string) (*Domain, error) {
	if name == "" {
		if len(c.domains) == 1 {
			return &c.domains[0], nil
		}
		return nil, fmt.Errorf("this server publishes %d domains (%s); name one",
			len(c.domains), strings.Join(c.Names(), ", "))
	}
	d, ok := c.byName[name]
	if !ok {
		return nil, fmt.Errorf("no domain named %q (published: %s)", name, strings.Join(c.Names(), ", "))
	}
	return d, nil
}

// MarshalJSON keeps the model out of a serialized domain while letting the
// routing catalog be dumped straight into a prompt.
func (d Domain) MarshalJSON() ([]byte, error) {
	type alias Domain
	return json.Marshal(alias(d))
}
