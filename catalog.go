package semantic

import "fmt"

// ---------------------------------------------------------------------------
// The lookup half of the contract: what exists, and at what grain.
//
// These functions open no database and compile no SQL. They answer the two
// questions an agent must be able to settle BEFORE it commits to a query —
// "which governed metrics might answer this" and "what may I slice them by" —
// so that picking from a validated menu replaces writing SQL and hoping.
// ---------------------------------------------------------------------------

// MetricInfo is one row of the metric catalog: enough for an agent to route a
// natural-language question to a governed metric, and to see the grain and
// aggregation it is committing to, without a second lookup.
type MetricInfo struct {
	Name          string   `json:"name"`
	Description   string   `json:"description,omitempty"`
	Synonyms      []string `json:"synonyms,omitempty"`
	Kind          string   `json:"kind"`                     // base | derived | window
	Aggregation   string   `json:"aggregation,omitempty"`    // base metrics only
	Formula       string   `json:"formula,omitempty"`        // derived metrics only
	Window        string   `json:"window,omitempty"`         // window metrics only
	NativeDataset string   `json:"native_dataset,omitempty"` // base metrics only
	Grain         []string `json:"grain,omitempty"`          // the native dataset's key tuple
	GrainStatus   string   `json:"grain_status,omitempty"`   // pass | fail | untested
	Additivity    string   `json:"additivity"`
	BaseMetrics   []string `json:"base_metrics,omitempty"` // what a derived/window metric rests on

	// Meta is the metric's free-form tag map, passed through so a caller can
	// key its own policy off it.
	Meta map[string]string `json:"meta,omitempty"`

	// OKFLinks is left for the caller to fill from its own frontmatter-derived
	// link table. It is identifiers only, never content: the point of the
	// boundary is that business context is fetched deliberately, in a second
	// call, when a caveat is actually relevant — not stapled onto every
	// catalog row and paid for on every question.
	OKFLinks []string `json:"okf_links,omitempty"`
}

// MetricFilter narrows a catalog listing.
type MetricFilter struct {
	// Dimensions, if set, keeps only metrics that can be sliced by every one of
	// them — the mirror of list_dimensions, for a caller that picked the
	// breakdown first. Resolved through synonyms; an unknown name is an error
	// from ListMetricsBy rather than a silently empty list.
	Dimensions []string

	// Roles are the caller's roles; restricted metrics they do not hold are
	// omitted entirely rather than listed and refused later.
	Roles []string
	// Meta requires each named tag to be present with this exact value
	// (e.g. {"agent_accessible": "true"}).
	Meta map[string]string
}

func (mf MetricFilter) admits(mt *Metric) bool {
	if !mt.VisibleTo(mf.Roles) {
		return false
	}
	for k, want := range mf.Meta {
		if mt.Meta[k] != want {
			return false
		}
	}
	return true
}

// ListMetrics returns the catalog rows a caller may see, in definition order.
//
// It enumerates rather than retrieves. At the scale a single governed domain
// actually reaches, returning the whole list is both cheaper and more reliable
// than a relevance pass that can rank the right metric off the end — and the
// failure modes are not comparable: a list that is slightly too long costs
// tokens, while a retrieval that drops the right metric produces a confident
// answer from the wrong one.
func (m *Model) ListMetrics(f MetricFilter) []MetricInfo {
	out, _ := m.ListMetricsBy(f)
	return out
}

// ListMetricsBy is ListMetrics with the dimension filter's errors surfaced: an
// unknown breakdown must not read as "no metric supports it".
func (m *Model) ListMetricsBy(f MetricFilter) ([]MetricInfo, error) {
	sliceable := map[string]bool{}
	if len(f.Dimensions) > 0 {
		names, err := m.MetricsFor(f.Dimensions, f.Roles)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			sliceable[n] = true
		}
	}
	out := make([]MetricInfo, 0, len(m.Metrics))
	for i := range m.Metrics {
		mt := &m.Metrics[i]
		if !f.admits(mt) {
			continue
		}
		if len(f.Dimensions) > 0 && !sliceable[mt.Name] {
			continue
		}
		out = append(out, m.metricInfo(mt))
	}
	return out, nil
}

// DescribeMetric returns one catalog row by name or synonym.
func (m *Model) DescribeMetric(name string, f MetricFilter) (MetricInfo, error) {
	canon, ok := m.ResolveMetricName(name)
	if !ok {
		if sugg := m.SuggestMetricNames(name, 3); len(sugg) > 0 {
			return MetricInfo{}, fmt.Errorf("unknown metric %q; did you mean %s?", name, humanList(sugg))
		}
		return MetricInfo{}, fmt.Errorf("unknown metric %q", name)
	}
	mt := m.Metric(canon)
	if !f.admits(mt) {
		return MetricInfo{}, &RoleError{Metric: canon, Needs: mt.Roles, Has: f.Roles}
	}
	return m.metricInfo(mt), nil
}

func (m *Model) metricInfo(mt *Metric) MetricInfo {
	info := MetricInfo{
		Name:        mt.Name,
		Description: mt.Description,
		Synonyms:    mt.Synonyms,
		Aggregation: mt.Agg,
		Formula:     mt.Formula,
		Window:      mt.Window,
		Additivity:  m.Additivity(mt.Name),
		Meta:        mt.Meta,
	}
	switch {
	case mt.IsWindow():
		info.Kind = "window"
	case mt.IsDerived():
		info.Kind = "derived"
	default:
		info.Kind = "base"
		info.NativeDataset = mt.Entity
		if e := m.Entity(mt.Entity); e != nil {
			info.Grain = e.PrimaryKey
			info.GrainStatus = e.GrainStatus
			if info.GrainStatus == "" {
				info.GrainStatus = Untested
			}
		}
	}
	if info.Kind != "base" {
		c := &compiler{m: m, baseSeen: map[string]bool{}}
		if err := c.collectBases(mt.Name, map[string]bool{}); err == nil {
			info.BaseMetrics = c.baseOrder
		}
	}
	return info
}
