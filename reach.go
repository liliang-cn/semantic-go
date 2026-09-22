package semantic

import (
	"fmt"
	"regexp"
	"strings"
)

// edge is one safe (many-to-one) traversal step in the join graph. The column
// lists are parallel and equal length — a composite key joins on all of it.
type edge struct {
	to        string
	leftCols  []string // columns on the "from" entity's table
	rightCols []string // columns on the "to" entity's table
}

// adjacency returns the many-to-one traversal graph, built once by Index and
// shared by the compiler (join planning) and reachability (valid-dimension
// discovery).
func (m *Model) adjacency() map[string][]edge {
	if m.adj != nil {
		return m.adj
	}
	return m.buildAdjacency()
}

func (m *Model) buildAdjacency() map[string][]edge {
	adj := map[string][]edge{}
	for _, j := range m.Joins {
		switch j.Cardinality {
		case "one_to_many":
			// many side = To, one side = From; safe edge To -> From
			adj[j.To] = append(adj[j.To], edge{to: j.From, leftCols: j.ToKey, rightCols: j.FromKey})
		case "many_to_one":
			// many side = From, one side = To; safe edge From -> To
			adj[j.From] = append(adj[j.From], edge{to: j.To, leftCols: j.FromKey, rightCols: j.ToKey})
		case "many_to_many":
			// not traversable directly — must go through a bridge entity
		}
	}
	return adj
}

// ReachableEntities returns the set of entities reachable from base by following
// only many-to-one edges (the joins that don't fan out the base grain).
//
// The result is memoized by Index and must be treated as read-only: it is
// shared, and the reachable set of a declared entity does not change while the
// model does not.
func (m *Model) ReachableEntities(base string) map[string]bool {
	if r, ok := m.reach[base]; ok {
		return r
	}
	return m.reachableFrom(base)
}

func (m *Model) reachableFrom(base string) map[string]bool {
	adj := m.adjacency()
	seen := map[string]bool{base: true}
	queue := []string{base}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, e := range adj[cur] {
			if !seen[e.to] {
				seen[e.to] = true
				queue = append(queue, e.to)
			}
		}
	}
	return seen
}

var metricRefRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// baseEntitiesOf collects the base-metric entities a metric depends on (recursing
// through derived formulas).
func (m *Model) baseEntitiesOf(name string, visiting map[string]bool) ([]string, error) {
	mt := m.Metric(name)
	if mt == nil {
		return nil, &UnknownMetricError{name}
	}
	if visiting[name] {
		return nil, nil
	}
	visiting[name] = true
	switch {
	case mt.IsWindow():
		// A window metric has no entity of its own: it is a transform of the
		// metric named by `of`, and it is sliceable exactly where that metric
		// is. Reading mt.Entity here (it is empty) made every window metric
		// report zero valid dimensions — the catalog offered them and refused
		// every way of grouping them.
		if mt.Of == "" {
			return nil, fmt.Errorf("window metric %q needs `of`", name)
		}
		return m.baseEntitiesOf(mt.Of, visiting)
	case mt.IsDerived():
		var out []string
		for _, tok := range metricRefRe.FindAllString(mt.Formula, -1) {
			if m.Metric(tok) == nil {
				continue // SQL function/literal
			}
			sub, err := m.baseEntitiesOf(tok, visiting)
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
		}
		return out, nil
	default:
		return []string{mt.Entity}, nil
	}
}

// DimensionsFor returns the dimensions that can slice the metric WITHOUT a fanout
// — i.e. dimensions whose entity is reachable from every base metric the metric
// depends on. (This is why "net_revenue by product_category" is rejected: refunds
// can't reach product.) Powers the get_dimensions tool so agents self-correct.
func (m *Model) DimensionsFor(metric string) ([]string, error) {
	bases, err := m.baseEntitiesOf(metric, map[string]bool{})
	if err != nil {
		return nil, err
	}
	if len(bases) == 0 {
		return nil, nil
	}
	inter := m.reachableIntersection(bases)
	var out []string
	for i := range m.Dimensions {
		if inter[m.Dimensions[i].Entity] {
			out = append(out, m.Dimensions[i].Name)
		}
	}
	return out, nil
}

// UnknownMetricError is returned when a metric name doesn't exist.
type UnknownMetricError struct{ Name string }

func (e *UnknownMetricError) Error() string { return "unknown metric " + e.Name }

// DimensionNames returns dimension names in definition order.
func (m *Model) DimensionNames() []string {
	out := make([]string, len(m.Dimensions))
	for i := range m.Dimensions {
		out[i] = m.Dimensions[i].Name
	}
	return out
}

// ---------------------------------------------------------------------------
// Why a dimension is unavailable
//
// DimensionsFor answers "what may I group by". That is enough to keep an agent
// safe and not enough to keep it cooperative: a dimension a reader plainly
// expects to be groupable, silently absent, reads as a gap in the model rather
// than a guardrail — and the reasonable next move is to go around the semantic
// layer and write the SQL by hand. So the layer also says, unprompted, which
// obvious-looking dimensions are excluded and which declared edge excludes them.
// ---------------------------------------------------------------------------

// undirectedStep is one hop of a path found without regard to join direction.
type undirectedStep struct {
	from, to string
	join     Join
	safe     bool   // traversable without multiplying the base grain
	why      string // when unsafe, what makes it so
}

// undirectedAdj returns the join graph ignoring direction, tagging each hop
// with whether traversing it THAT way is grain-safe. Built once by Index.
func (m *Model) undirectedAdj() map[string][]undirectedStep {
	if m.undirAdj != nil {
		return m.undirAdj
	}
	return m.buildUndirectedAdj()
}

func (m *Model) buildUndirectedAdj() map[string][]undirectedStep {
	adj := map[string][]undirectedStep{}
	for _, j := range m.Joins {
		fwd := undirectedStep{from: j.From, to: j.To, join: j}
		rev := undirectedStep{from: j.To, to: j.From, join: j}
		switch j.Cardinality {
		case "many_to_one":
			fwd.safe = true
			rev.why = fmt.Sprintf("%s is one-to-many onto %s", j.To, j.From)
		case "one_to_many":
			rev.safe = true
			fwd.why = fmt.Sprintf("%s is one-to-many onto %s", j.From, j.To)
		case "many_to_many":
			fwd.why = fmt.Sprintf("%s ↔ %s is many-to-many", j.From, j.To)
			rev.why = fwd.why
		}
		adj[j.From] = append(adj[j.From], fwd)
		adj[j.To] = append(adj[j.To], rev)
	}
	return adj
}

// maxExplainHops bounds the search for an explanation. The unavailable list is
// not a response to a request — nothing bounds it by what was asked — so it is
// bounded by rule instead: a dimension more than this many joins away from the
// measure is not something a reader expected to be able to group by, and naming
// it would turn a short, useful list into an inventory of the whole model.
const maxExplainHops = 3

// pathTo finds the shortest undirected path from base to target, or nil.
func (m *Model) pathTo(base, target string, maxHops int) []undirectedStep {
	if base == target {
		return []undirectedStep{}
	}
	adj := m.undirectedAdj()
	type node struct {
		at   string
		path []undirectedStep
	}
	seen := map[string]bool{base: true}
	queue := []node{{at: base}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if len(cur.path) >= maxHops {
			continue
		}
		for _, st := range adj[cur.at] {
			if seen[st.to] {
				continue
			}
			p := append(append([]undirectedStep{}, cur.path...), st)
			if st.to == target {
				return p
			}
			seen[st.to] = true
			queue = append(queue, node{at: st.to, path: p})
		}
	}
	return nil
}

// describePath renders a path as "a → b → c" plus the first unsafe hop's reason.
func describePath(p []undirectedStep) (route string, why string) {
	if len(p) == 0 {
		return "", ""
	}
	names := []string{p[0].from}
	for _, st := range p {
		names = append(names, st.to)
	}
	for _, st := range p {
		if !st.safe {
			return strings.Join(names, " → "), st.why
		}
	}
	return strings.Join(names, " → "), ""
}

// explainUnreachable appends a short clause naming the path that exists but
// cannot be taken, so a refusal tells the modeller which edge to look at.
func (m *Model) explainUnreachable(base, target string) string {
	p := m.pathTo(base, target, maxExplainHops)
	if len(p) == 0 {
		return ""
	}
	route, why := describePath(p)
	if why == "" {
		return ""
	}
	return fmt.Sprintf(": the only path within %d joins is %s, and %s", maxExplainHops, route, why)
}

// DimAvailability describes one dimension a set of metrics may be sliced by.
type DimAvailability struct {
	Name        string `json:"name"`
	Entity      string `json:"dataset"`
	IsTime      bool   `json:"is_time,omitempty"`
	Join        string `json:"join"`                  // "native", or "a.key → b.key"
	Cardinality string `json:"cardinality,omitempty"` // of the final hop
	GrainSafe   bool   `json:"grain_safe"`
}

// DimExclusion names a dimension that is NOT groupable with these metrics, and
// the declared edge that makes it unsafe. One entry per dimension, listing
// every requested metric it is unavailable for — the reader is deciding about
// a dimension, not about a metric×dimension pair.
type DimExclusion struct {
	Name    string   `json:"name"`
	Entity  string   `json:"dataset"`
	Metrics []string `json:"metrics"` // the requested metrics it is unavailable for
	Reason  string   `json:"reason"`
}

// DimensionReport answers list_dimensions for a SET of metrics at once: the
// intersection of what every metric can legally be sliced by, plus the
// pre-emptive exclusion list described above.
//
// Called with all the metrics a question needs, not one at a time: a dimension
// that is safe for hours and unsafe for cost is unsafe for a query that asks
// for both, and discovering that one metric at a time means discovering it
// after committing to a plan.
func (m *Model) DimensionReport(metrics []string, roles []string) ([]DimAvailability, []DimExclusion, error) {
	if len(metrics) == 0 {
		return nil, nil, fmt.Errorf("list_dimensions needs at least one metric")
	}
	// Bases, per requested metric, so an exclusion can name which metric it is
	// an exclusion for.
	basesOf := map[string][]string{}
	var allBases []string
	for _, name := range metrics {
		if err := m.checkRoles(name, roles, map[string]bool{}); err != nil {
			return nil, nil, err
		}
		b, err := m.baseEntitiesOf(name, map[string]bool{})
		if err != nil {
			return nil, nil, err
		}
		if len(b) == 0 {
			return nil, nil, fmt.Errorf("metric %q resolves to no base measure", name)
		}
		basesOf[name] = b
		allBases = append(allBases, b...)
	}

	// Intersection of reachable entities across every base of every metric,
	// and each base's own reachable set, computed once rather than once per
	// dimension examined below.
	inter := m.reachableIntersection(allBases)
	reachOf := map[string]map[string]bool{}
	for _, b := range allBases {
		reachOf[b] = m.ReachableEntities(b)
	}

	var avail []DimAvailability
	var excl []DimExclusion
	seenExcl := map[string]bool{}
	for i := range m.Dimensions {
		d := &m.Dimensions[i]
		if inter[d.Entity] {
			join, card := m.joinOf(allBases, d.Entity)
			avail = append(avail, DimAvailability{
				Name:        d.Name,
				Entity:      d.Entity,
				IsTime:      d.Type == "time",
				Join:        join,
				Cardinality: card,
				GrainSafe:   true,
			})
			continue
		}
		// Not available. Report it only when it is excluded for a grain-safety
		// reason within the hop bound — an unrelated dimension is not a
		// guardrail a reader would mistake for a gap.
		var blocked []string
		reason := ""
		for _, metric := range metrics {
			for _, base := range basesOf[metric] {
				if reachOf[base][d.Entity] {
					continue // safe for this base; another base is what excluded it
				}
				route, why := describePath(m.pathTo(base, d.Entity, maxExplainHops))
				if why == "" {
					continue // unrelated, or reachable safely: not a grain-safety exclusion
				}
				if !seenExcl[d.Name+"\x00"+metric] {
					seenExcl[d.Name+"\x00"+metric] = true
					blocked = append(blocked, metric)
				}
				if reason == "" {
					reason = fmt.Sprintf("requires path %s, and %s — so aggregating %s across it would multiply the measure",
						route, why, metric)
				}
			}
		}
		if len(blocked) > 0 {
			excl = append(excl, DimExclusion{Name: d.Name, Entity: d.Entity, Metrics: blocked, Reason: reason})
		}
	}
	return avail, excl, nil
}

// joinOf renders how a dimension's entity is reached from the metrics' native
// datasets — "native" when it is one of them, otherwise the final hop — and the
// cardinality of that hop. One walk, not two: these were separate functions
// that each ran the same search.
func (m *Model) joinOf(bases []string, target string) (join, cardinality string) {
	for _, b := range bases {
		if b == target {
			return "native", ""
		}
	}
	for _, b := range bases {
		if p := m.pathTo(b, target, maxExplainHops); len(p) > 0 {
			last := p[len(p)-1]
			return fmt.Sprintf("%s.%s → %s.%s", last.from, keyOn(last, last.from), last.to, keyOn(last, last.to)), "many_to_one"
		}
	}
	return "", ""
}

// reachableIntersection returns the entities reachable from EVERY base, as a
// fresh map — the memoized per-base sets are shared and must not be narrowed
// in place.
func (m *Model) reachableIntersection(bases []string) map[string]bool {
	if len(bases) == 0 {
		return nil
	}
	inter := map[string]bool{}
	for k := range m.ReachableEntities(bases[0]) {
		inter[k] = true
	}
	for _, b := range bases[1:] {
		r := m.ReachableEntities(b)
		for k := range inter {
			if !r[k] {
				delete(inter, k)
			}
		}
	}
	return inter
}

// keyOn returns the join key belonging to entity `ent` on this hop.
func keyOn(st undirectedStep, ent string) string {
	if ent == st.join.From {
		return st.join.FromKey.String()
	}
	return st.join.ToKey.String()
}

// ---------------------------------------------------------------------------
// The other direction
//
// DimensionsFor answers "what may I slice this metric by". A BI surface — and
// an agent building one — needs the mirror of that too: the user picks a
// breakdown first ("by region", "by site category") and wants to know which
// numbers can honestly be shown against it.
//
// Asking the first question once per metric gives the same answer, at the cost
// of one round trip per metric and a caller that has to intersect the results
// itself. Worse, a caller that gets it wrong intersects the wrong way and
// offers a metric that will be refused when it is finally asked for.
// ---------------------------------------------------------------------------

// MetricsFor returns the metrics that can be sliced by EVERY one of the given
// dimensions, in definition order, hiding any the caller's roles do not admit.
//
// Empty dimensions means "no breakdown", which every metric supports — except
// that a semi-additive measure cannot be totalled across all of time, so the
// honest answer there is still to name the dimensions.
func (m *Model) MetricsFor(dimensions []string, roles []string) ([]string, error) {
	entities := make([]string, 0, len(dimensions))
	for _, name := range dimensions {
		canon, ok := m.ResolveDimensionName(name)
		if !ok {
			return nil, fmt.Errorf("unknown dimension %q (known: %s)", name, summarizeNames(m.DimensionNames()))
		}
		entities = append(entities, m.Dimension(canon).Entity)
	}

	var out []string
	for i := range m.Metrics {
		mt := &m.Metrics[i]
		if !mt.VisibleTo(roles) {
			continue
		}
		bases, err := m.baseEntitiesOf(mt.Name, map[string]bool{})
		if err != nil || len(bases) == 0 {
			continue // a broken metric is Lint's to report, not this listing's
		}
		if reachesAll(m, bases, entities) {
			out = append(out, mt.Name)
		}
	}
	return out, nil
}

// reachesAll reports whether every base measure can reach every dimension's
// dataset without fanning out.
func reachesAll(m *Model, bases, entities []string) bool {
	for _, base := range bases {
		r := m.ReachableEntities(base)
		for _, e := range entities {
			if !r[e] {
				return false
			}
		}
	}
	return true
}

// MetricReport is the mirror of DimensionReport: given a breakdown, which
// metrics can honestly be shown against it, and which cannot and why.
func (m *Model) MetricReport(dimensions []string, roles []string) ([]MetricInfo, []MetricExclusion, error) {
	allowed, err := m.MetricsFor(dimensions, roles)
	if err != nil {
		return nil, nil, err
	}
	ok := map[string]bool{}
	for _, n := range allowed {
		ok[n] = true
	}

	var avail []MetricInfo
	var excl []MetricExclusion
	for i := range m.Metrics {
		mt := &m.Metrics[i]
		if !mt.VisibleTo(roles) {
			continue
		}
		if ok[mt.Name] {
			avail = append(avail, m.metricInfo(mt))
			continue
		}
		if reason := m.whyNotSliceable(mt.Name, dimensions); reason != "" {
			excl = append(excl, MetricExclusion{Name: mt.Name, Reason: reason})
		}
	}
	return avail, excl, nil
}

// MetricExclusion names a metric that cannot be shown against a breakdown, and
// the declared edge that prevents it.
type MetricExclusion struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// whyNotSliceable names the first dimension a metric cannot reach, and the
// edge that stops it. Returns "" when the metric is fine or is broken in a way
// Lint owns.
func (m *Model) whyNotSliceable(metric string, dimensions []string) string {
	bases, err := m.baseEntitiesOf(metric, map[string]bool{})
	if err != nil || len(bases) == 0 {
		return ""
	}
	for _, name := range dimensions {
		canon, ok := m.ResolveDimensionName(name)
		if !ok {
			continue
		}
		target := m.Dimension(canon).Entity
		for _, base := range bases {
			if m.ReachableEntities(base)[target] {
				continue
			}
			route, why := describePath(m.pathTo(base, target, maxExplainHops))
			if why != "" {
				return fmt.Sprintf("cannot be sliced by %q: it lives on %s, and reaching %s requires path %s, where %s",
					canon, base, target, route, why)
			}
			return fmt.Sprintf("cannot be sliced by %q: no declared join path from %s to %s",
				canon, base, target)
		}
	}
	return ""
}
