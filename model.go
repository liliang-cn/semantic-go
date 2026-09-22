// Package semantic is a dependency-light semantic layer for Go: a model
// (entities, dimensions, metrics, join graph) plus a compiler that turns a
// semantic Query — "this metric by these dimensions" — into fanout/chasm-safe
// SQL. It speaks no LLM, opens no database; it only produces SQL strings.
//
// The core technique (the reason this exists): aggregate each measure to its
// base grain inside a CTE first, THEN join dimensions. That single move makes
// fan-out and chasm joins impossible by construction.
package semantic

import (
	"fmt"
	"strings"
)

// Model is the single source of truth: business meaning compiled to SQL once.
type Model struct {
	// Name and Description identify the model as a published DOMAIN. A server
	// may publish several, and routing to the right one happens before anything
	// else — off Description alone, with no round trip. A domain without one is
	// a line in that list that says only its file name, which is the difference
	// between routing and guessing.
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	// Instructions is free text handed to the agent alongside the catalog —
	// the interchange format's ai_context.instructions.
	Instructions string `yaml:"instructions,omitempty"`

	Entities   []Entity    `yaml:"entities"`
	Joins      []Join      `yaml:"joins"`
	Dimensions []Dimension `yaml:"dimensions"`
	Metrics    []Metric    `yaml:"metrics"`

	entity    map[string]*Entity
	dimension map[string]*Dimension
	metric    map[string]*Metric

	// metricSyn maps a normalized synonym (or canonical name) to the canonical
	// metric name, so a caller may name a metric by any declared synonym.
	metricSyn map[string]string

	// dimSyn maps a normalized synonym (or canonical name) to the canonical
	// dimension name — the dimension analogue of metricSyn.
	dimSyn map[string]string

	// Join graphs, built once by Index. They were being rebuilt on every call
	// that consulted them, and the dimension report consults them once per
	// (dimension × metric × base measure): a pure metadata lookup cost half a
	// millisecond and 1.4MB of garbage, seventeen times what compiling the
	// actual query cost. A Model is read-only after Index, so there is nothing
	// for the cache to go stale against.
	adj      map[string][]edge
	undirAdj map[string][]undirectedStep

	// reach memoizes ReachableEntities per base entity.
	reach map[string]map[string]bool
}

// Entity is a real business thing (the "dataset" of a semantic interchange
// document) with a declared grain the layer joins on — the key is declared,
// never guessed.
//
// PrimaryKey is the grain: the column tuple that is unique in the table. It
// accepts a scalar (`primary_key: order_id`) or a sequence
// (`primary_key: [guard_id, shift_id]`) in YAML.
type Entity struct {
	Name       string     `yaml:"name"`
	Table      string     `yaml:"table"`
	PrimaryKey StringList `yaml:"primary_key"`

	// Grain is the human sentence a modeller writes next to the key tuple —
	// "one row per (guard_id, shift_id)". It is documentation for the agent;
	// PrimaryKey is what the layer enforces.
	Grain string `yaml:"grain,omitempty"`

	// GrainStatus records what the build-time uniqueness test found:
	//   "pass"  — the key tuple was verified unique; the entity is trusted.
	//   "fail"  — duplicates were found; the compiler REFUSES to use it.
	//   ""      — never run. The model still compiles (so a model can be
	//             authored before its test exists), but Lint says so.
	// Grain is validated at model-build time, never at query time: the query
	// path should never be the first thing to discover a grain problem.
	GrainStatus string `yaml:"grain_status,omitempty"`
}

// Join is one declared edge of the join graph: keys + cardinality. The compiler
// only ever traverses declared edges in the safe (many-to-one) direction; a
// missing edge is refused, never invented.
type Join struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
	// FromKey and ToKey accept a scalar or a list: a composite foreign key
	// (`from_key: [tenant_id, order_id]`) joins on every column at once.
	// Getting this wrong is not a syntax error downstream — a composite key
	// joined on one of its columns fans out silently — so the lengths must
	// match, and Index checks that they do.
	FromKey     StringList `yaml:"from_key"`
	ToKey       StringList `yaml:"to_key"`
	Cardinality string     `yaml:"cardinality"` // many_to_one | one_to_many | many_to_many
}

// Dimension is a typed attribute to group/filter by, named in business words.
type Dimension struct {
	Name     string   `yaml:"name"`
	Entity   string   `yaml:"entity"`
	Column   string   `yaml:"column"`
	Type     string   `yaml:"type"` // categorical | time
	Synonyms []string `yaml:"synonyms,omitempty"`

	// Roles, if set, are the roles that may see this dimension's raw values.
	// A caller without one of them still gets the dimension — masked.
	Roles []string `yaml:"roles,omitempty"`

	// Mask is the SQL expression substituted for the column when the caller may
	// not see the raw value, e.g. "'***'" or "left(email, 1) || '***'".
	//
	// Masking is a projection, not a filter: the rows are still there and the
	// measures over them are still right, so "revenue by customer" stays a
	// truthful total while the names do not leave the warehouse. That is why
	// masking exists alongside Roles on a metric, which removes the number
	// entirely — they answer different questions.
	Mask string `yaml:"mask,omitempty"`
}

// visibleTo reports whether a caller holding these roles may see raw values.
// A dimension with no Roles is public; one with Roles and no Mask is a
// modelling error caught at Index time, since there would be nothing to show a
// caller who lacks them.
func (d *Dimension) visibleTo(roles []string) bool {
	if len(d.Roles) == 0 {
		return true
	}
	for _, want := range d.Roles {
		for _, got := range roles {
			if strings.EqualFold(want, got) {
				return true
			}
		}
	}
	return false
}

// Metric is an aggregated number with grain + aggregation locked in. A simple
// metric aggregates Expr over its base Entity; a derived metric is a formula
// over other metric names (e.g. "total_revenue - refund_total") — which is how
// chasm traps are avoided: each base metric aggregates in its own CTE.
type Metric struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Synonyms    []string `yaml:"synonyms,omitempty"`

	// base metric
	Entity string `yaml:"entity,omitempty"`
	Agg    string `yaml:"agg,omitempty"`  // sum | count | count_distinct | avg | min | max
	Expr   string `yaml:"expr,omitempty"` // SQL expr at base grain, e.g. "quantity * unit_price"

	// derived metric
	Formula string `yaml:"formula,omitempty"` // expression over metric names

	// time-window metric: a transform of metric Of over the time dimension.
	// Window is one of: rolling:N | cumulative | prior:N | delta:N
	Of     string `yaml:"of,omitempty"`
	Window string `yaml:"window,omitempty"`
	// Reset gives a window metric grain-to-date semantics: the accumulation
	// restarts at each boundary of this period (e.g. reset: year → YTD). Empty
	// means the window runs unbroken across the whole series.
	// One of: day | week | month | quarter | year.
	Reset string `yaml:"reset,omitempty"`

	// Additivity declares how the measure may be rolled up, so the compiler can
	// refuse a roll-up that would produce a silent wrong number:
	//   additive      — safe to sum across any dimension (revenue, units).
	//   semi_additive — summable across non-time dims only; over time use a
	//                    point-in-time pick, never SUM (inventory, balances).
	//   non_additive  — never summable (ratios, distinct counts).
	// Empty means "infer from agg/formula" (see EffectiveAdditivity).
	Additivity string `yaml:"additivity,omitempty"`

	// governance
	Roles []string `yaml:"roles,omitempty"` // if set, only these roles may resolve the metric

	// Meta carries free-form tags the catalog can filter on — the meta_filter
	// pattern: a deployment that only wants agents to see vetted metrics tags
	// them `agent_accessible: "true"` and asks ListMetrics for that. Unlike
	// Roles it is not a security boundary; it shapes what is offered, not what
	// is permitted, and Compile does not consult it.
	Meta map[string]string `yaml:"meta,omitempty"`
}

func (m *Metric) IsDerived() bool { return m.Formula != "" }
func (m *Metric) IsWindow() bool  { return m.Window != "" }

// Additivity classes.
const (
	Additive     = "additive"
	SemiAdditive = "semi_additive"
	NonAdditive  = "non_additive"
)

// Grain test outcomes recorded on an entity by the build-time gate.
const (
	GrainPass = "pass"
	GrainFail = "fail"
)

// validPeriod reports whether s names a date_trunc period the layer supports.
func validPeriod(s string) bool {
	switch s {
	case "day", "week", "month", "quarter", "year":
		return true
	}
	return false
}

// Index builds lookups and validates references. Must be called after loading.
func (m *Model) Index() error {
	m.entity = map[string]*Entity{}
	m.dimension = map[string]*Dimension{}
	m.metric = map[string]*Metric{}

	// Duplicate declared names used to load without complaint: the later one
	// won the index and the earlier one became unreachable — present in the
	// file, listed by nothing, resolvable by nobody, and which of the two
	// survived was an accident of file order.
	for i := range m.Entities {
		e := &m.Entities[i]
		if e.Name == "" || e.Table == "" || len(e.PrimaryKey) == 0 {
			return fmt.Errorf("entity %q: name, table and primary_key are required", e.Name)
		}
		if _, dup := m.entity[e.Name]; dup {
			return fmt.Errorf("two entities are both named %q: one of them could never be referenced", e.Name)
		}
		for _, k := range e.PrimaryKey {
			if strings.TrimSpace(k) == "" {
				return fmt.Errorf("entity %q: primary_key contains an empty column", e.Name)
			}
		}
		switch e.GrainStatus {
		case "", GrainPass, GrainFail:
		default:
			return fmt.Errorf("entity %q: bad grain_status %q (pass|fail, or empty for not-yet-tested)", e.Name, e.GrainStatus)
		}
		m.entity[e.Name] = e
	}
	for i := range m.Dimensions {
		d := &m.Dimensions[i]
		if m.entity[d.Entity] == nil {
			return fmt.Errorf("dimension %q references unknown entity %q", d.Name, d.Entity)
		}
		if d.Type != "categorical" && d.Type != "time" {
			return fmt.Errorf("dimension %q: type must be categorical or time", d.Name)
		}
		if len(d.Roles) > 0 && d.Mask == "" {
			return fmt.Errorf("dimension %q restricts roles but declares no mask: there would be nothing to show a "+
				"caller who lacks them. Add `mask:` (e.g. \"'***'\"), or drop the roles and restrict the metric instead", d.Name)
		}
		if _, dup := m.dimension[d.Name]; dup {
			return fmt.Errorf("two dimensions are both named %q: one of them could never be grouped by", d.Name)
		}
		if d.Mask != "" && len(d.Roles) == 0 {
			return fmt.Errorf("dimension %q declares a mask but no roles, so it would be masked for everybody "+
				"including the people it is meant for — add `roles:`", d.Name)
		}
		m.dimension[d.Name] = d
	}
	for i := range m.Metrics {
		mt := &m.Metrics[i]
		if mt.Name == "" {
			return fmt.Errorf("a metric has no name")
		}
		if _, dup := m.metric[mt.Name]; dup {
			return fmt.Errorf("two metrics are both named %q: one of them could never be asked for", mt.Name)
		}
		// A name that is both a metric and a dimension puts two columns called
		// the same thing in one result. Which one a caller reads back depends on
		// its driver, and the dimension is the one that loses quietly.
		if m.dimension[mt.Name] != nil {
			return fmt.Errorf("%q is both a metric and a dimension: the result would carry two columns under that "+
				"one name, and which of them a caller reads back would depend on its driver", mt.Name)
		}
		if !mt.IsDerived() && !mt.IsWindow() && m.entity[mt.Entity] == nil {
			return fmt.Errorf("metric %q references unknown entity %q", mt.Name, mt.Entity)
		}
		switch mt.Additivity {
		case "", Additive, SemiAdditive, NonAdditive:
		default:
			return fmt.Errorf("metric %q: bad additivity %q (additive|semi_additive|non_additive)", mt.Name, mt.Additivity)
		}
		if mt.Reset != "" {
			if !mt.IsWindow() {
				return fmt.Errorf("metric %q: reset is only valid on a window metric", mt.Name)
			}
			if !validPeriod(mt.Reset) {
				return fmt.Errorf("metric %q: bad reset %q (day|week|month|quarter|year)", mt.Name, mt.Reset)
			}
		}
		m.metric[mt.Name] = mt
	}
	// Synonym index: declared synonyms first (first metric wins a shared
	// synonym), then canonical names last so a name always beats a synonym.
	m.metricSyn = map[string]string{}
	for i := range m.Metrics {
		for _, syn := range m.Metrics[i].Synonyms {
			k := normName(syn)
			if k == "" {
				continue
			}
			if _, taken := m.metricSyn[k]; !taken {
				m.metricSyn[k] = m.Metrics[i].Name
			}
		}
	}
	for i := range m.Metrics {
		m.metricSyn[normName(m.Metrics[i].Name)] = m.Metrics[i].Name
	}
	// Dimension synonym index (same precedence: declared synonyms first, then
	// canonical names, so a name always beats a synonym).
	m.dimSyn = map[string]string{}
	for i := range m.Dimensions {
		for _, syn := range m.Dimensions[i].Synonyms {
			k := normName(syn)
			if k == "" {
				continue
			}
			if _, taken := m.dimSyn[k]; !taken {
				m.dimSyn[k] = m.Dimensions[i].Name
			}
		}
	}
	for i := range m.Dimensions {
		m.dimSyn[normName(m.Dimensions[i].Name)] = m.Dimensions[i].Name
	}
	// A window over a window emits nested window functions, which no engine
	// accepts. Until now this was caught only incidentally — the inferred
	// additivity of a window result is non_additive, and summing a non-additive
	// measure is refused — so declaring `additivity: additive` on one walked
	// straight past the guard and produced SUM(SUM(…) OVER (…)) OVER (…).
	for i := range m.Metrics {
		if !m.Metrics[i].IsWindow() {
			continue
		}
		if err := m.checkWindowChain(m.Metrics[i].Name, map[string]bool{}); err != nil {
			return err
		}
	}

	m.adj, m.undirAdj, m.reach = nil, nil, nil

	for _, j := range m.Joins {
		if m.entity[j.From] == nil || m.entity[j.To] == nil {
			return fmt.Errorf("join %s->%s references an unknown entity", j.From, j.To)
		}
		switch j.Cardinality {
		case "many_to_one", "one_to_many", "many_to_many":
		default:
			return fmt.Errorf("join %s->%s: bad cardinality %q", j.From, j.To, j.Cardinality)
		}
		if len(j.FromKey) == 0 || len(j.ToKey) == 0 {
			return fmt.Errorf("join %s->%s: from_key and to_key are required", j.From, j.To)
		}
		if len(j.FromKey) != len(j.ToKey) {
			return fmt.Errorf("join %s->%s: from_key has %d column(s) and to_key has %d — "+
				"a composite key must be joined on all of its columns, or it fans out",
				j.From, j.To, len(j.FromKey), len(j.ToKey))
		}
	}

	m.adj = m.buildAdjacency()
	m.undirAdj = m.buildUndirectedAdj()
	m.reach = map[string]map[string]bool{}
	for i := range m.Entities {
		m.reach[m.Entities[i].Name] = m.reachableFrom(m.Entities[i].Name)
	}
	return nil
}

func (m *Model) Entity(name string) *Entity       { return m.entity[name] }
func (m *Model) Dimension(name string) *Dimension { return m.dimension[name] }
func (m *Model) Metric(name string) *Metric       { return m.metric[name] }

// Additivity resolves how a metric may be rolled up, honoring an explicit
// `additivity:` and otherwise inferring it from the metric's shape:
//   - base: sum/count/min/max → additive; count_distinct/avg → non_additive.
//   - derived: a ratio formula (uses / or %) → non_additive; otherwise the
//     least-additive of its parts (non_additive < semi_additive < additive).
//   - window: rolling/cumulative re-sum their input → non_additive; prior/delta
//     are differences → non_additive. (A window result is never re-summable.)
func (m *Model) Additivity(name string) string {
	mt := m.metric[name]
	if mt == nil {
		return Additive
	}
	if mt.Additivity != "" {
		return mt.Additivity
	}
	switch {
	case mt.IsWindow():
		return NonAdditive
	case mt.IsDerived():
		if strings.ContainsAny(mt.Formula, "/%") {
			return NonAdditive
		}
		worst := Additive
		identRe.ReplaceAllStringFunc(mt.Formula, func(tok string) string {
			if m.metric[tok] != nil {
				worst = leastAdditive(worst, m.Additivity(tok))
			}
			return tok
		})
		return worst
	default:
		switch strings.ToLower(mt.Agg) {
		case "count_distinct", "avg":
			return NonAdditive
		default:
			return Additive
		}
	}
}

// leastAdditive returns the more restrictive of two additivity classes.
func leastAdditive(a, b string) string {
	rank := func(s string) int {
		switch s {
		case NonAdditive:
			return 0
		case SemiAdditive:
			return 1
		default:
			return 2
		}
	}
	if rank(b) < rank(a) {
		return b
	}
	return a
}

// MetricNames returns metric names in definition order (for list_metrics tools).
func (m *Model) MetricNames() []string {
	out := make([]string, len(m.Metrics))
	for i := range m.Metrics {
		out[i] = m.Metrics[i].Name
	}
	return out
}

// VisibleTo reports whether a caller holding these roles may resolve the metric.
// A metric with no `roles:` is public. A metric that declares roles is refused
// when the caller presents none — a caller that forgot to pass its role context
// gets an error, never a silent leak.
func (mt *Metric) VisibleTo(roles []string) bool {
	if len(mt.Roles) == 0 {
		return true
	}
	for _, want := range mt.Roles {
		for _, got := range roles {
			if strings.EqualFold(want, got) {
				return true
			}
		}
	}
	return false
}

// RoleError is returned when a metric exists but the caller's roles do not
// admit it. It is deliberately distinct from "unknown metric": the catalog
// tools hide restricted metrics, so an agent asking for one by name is either
// out of date or probing, and the caller decides which message to surface.
type RoleError struct {
	Metric string
	Needs  []string
	Has    []string
}

func (e *RoleError) Error() string {
	return fmt.Sprintf("metric %q requires one of roles %v (caller has %v)", e.Metric, e.Needs, e.Has)
}

// VisibleMetricNames returns the metric names a caller with these roles may
// see, in definition order — the list backing list_metrics.
func (m *Model) VisibleMetricNames(roles []string) []string {
	out := make([]string, 0, len(m.Metrics))
	for i := range m.Metrics {
		if m.Metrics[i].VisibleTo(roles) {
			out = append(out, m.Metrics[i].Name)
		}
	}
	return out
}

// checkRoles walks a metric and everything it depends on (derived formulas,
// window `of`) and refuses if any part is role-restricted beyond the caller.
// Checking transitively matters: a public metric whose formula references a
// restricted one would otherwise launder it into view.
func (m *Model) checkRoles(name string, roles []string, seen map[string]bool) error {
	if seen[name] {
		return nil
	}
	seen[name] = true
	mt := m.Metric(name)
	if mt == nil {
		return nil // unknown metric is reported by the caller, with suggestions
	}
	if !mt.VisibleTo(roles) {
		return &RoleError{Metric: name, Needs: mt.Roles, Has: roles}
	}
	switch {
	case mt.IsWindow():
		return m.checkRoles(mt.Of, roles, seen)
	case mt.IsDerived():
		var rerr error
		identRe.ReplaceAllStringFunc(mt.Formula, func(tok string) string {
			if m.Metric(tok) != nil && rerr == nil {
				rerr = m.checkRoles(tok, roles, seen)
			}
			return tok
		})
		return rerr
	}
	return nil
}

// checkWindowChain walks a window metric's `of` and refuses another window on
// the way down.
func (m *Model) checkWindowChain(name string, seen map[string]bool) error {
	mt := m.metric[name]
	if mt == nil || !mt.IsWindow() {
		return nil
	}
	if seen[name] {
		return fmt.Errorf("window metric %q is defined in terms of itself", name)
	}
	seen[name] = true
	if mt.Of == "" {
		return fmt.Errorf("window metric %q needs `of`", name)
	}
	next := m.metric[mt.Of]
	if next == nil {
		return fmt.Errorf("window metric %q accumulates %q, which is not a metric", name, mt.Of)
	}
	if next.IsWindow() {
		return fmt.Errorf("window metric %q is a window over window metric %q: nesting them emits a window function "+
			"inside another, which no engine accepts. Point `of` at a base or derived metric, "+
			"or define %q over the same base directly", name, mt.Of, name)
	}
	return m.checkWindowChain(mt.Of, seen)
}
