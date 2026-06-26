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
	Entities   []Entity    `yaml:"entities"`
	Joins      []Join      `yaml:"joins"`
	Dimensions []Dimension `yaml:"dimensions"`
	Metrics    []Metric    `yaml:"metrics"`

	entity    map[string]*Entity
	dimension map[string]*Dimension
	metric    map[string]*Metric
}

// Entity is a real business thing with a primary key the layer joins on — the
// key is declared, never guessed.
type Entity struct {
	Name       string `yaml:"name"`
	Table      string `yaml:"table"`
	PrimaryKey string `yaml:"primary_key"`
}

// Join is one declared edge of the join graph: keys + cardinality. The compiler
// only ever traverses declared edges in the safe (many-to-one) direction; a
// missing edge is refused, never invented.
type Join struct {
	From        string `yaml:"from"`
	To          string `yaml:"to"`
	FromKey     string `yaml:"from_key"`
	ToKey       string `yaml:"to_key"`
	Cardinality string `yaml:"cardinality"` // many_to_one | one_to_many | many_to_many
}

// Dimension is a typed attribute to group/filter by, named in business words.
// Mask is the SQL expression returned when the caller may not see the raw value.
type Dimension struct {
	Name   string `yaml:"name"`
	Entity string `yaml:"entity"`
	Column string `yaml:"column"`
	Type   string `yaml:"type"` // categorical | time
	Mask   string `yaml:"mask"`
}

// Metric is an aggregated number with grain + aggregation locked in. A simple
// metric aggregates Expr over its base Entity; a derived metric is a formula
// over other metric names (e.g. "total_revenue - refund_total") — which is how
// chasm traps are avoided: each base metric aggregates in its own CTE.
type Metric struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Synonyms    []string `yaml:"synonyms"`

	// base metric
	Entity string `yaml:"entity"`
	Agg    string `yaml:"agg"`  // sum | count | count_distinct | avg | min | max
	Expr   string `yaml:"expr"` // SQL expr at base grain, e.g. "quantity * unit_price"

	// derived metric
	Formula string `yaml:"formula"` // expression over metric names

	// time-window metric: a transform of metric Of over the time dimension.
	// Window is one of: rolling:N | cumulative | prior:N | delta:N
	Of     string `yaml:"of"`
	Window string `yaml:"window"`
	// Reset gives a window metric grain-to-date semantics: the accumulation
	// restarts at each boundary of this period (e.g. reset: year → YTD). Empty
	// means the window runs unbroken across the whole series.
	// One of: day | week | month | quarter | year.
	Reset string `yaml:"reset"`

	// Additivity declares how the measure may be rolled up, so the compiler can
	// refuse a roll-up that would produce a silent wrong number:
	//   additive      — safe to sum across any dimension (revenue, units).
	//   semi_additive — summable across non-time dims only; over time use a
	//                    point-in-time pick, never SUM (inventory, balances).
	//   non_additive  — never summable (ratios, distinct counts).
	// Empty means "infer from agg/formula" (see EffectiveAdditivity).
	Additivity string `yaml:"additivity"`

	// governance
	Roles []string `yaml:"roles"` // if set, only these roles may resolve the metric
}

func (m *Metric) IsDerived() bool { return m.Formula != "" }
func (m *Metric) IsWindow() bool  { return m.Window != "" }

// Additivity classes.
const (
	Additive     = "additive"
	SemiAdditive = "semi_additive"
	NonAdditive  = "non_additive"
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

	for i := range m.Entities {
		e := &m.Entities[i]
		if e.Name == "" || e.Table == "" || e.PrimaryKey == "" {
			return fmt.Errorf("entity %q: name, table and primary_key are required", e.Name)
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
		m.dimension[d.Name] = d
	}
	for i := range m.Metrics {
		mt := &m.Metrics[i]
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
	for _, j := range m.Joins {
		if m.entity[j.From] == nil || m.entity[j.To] == nil {
			return fmt.Errorf("join %s->%s references an unknown entity", j.From, j.To)
		}
		switch j.Cardinality {
		case "many_to_one", "one_to_many", "many_to_many":
		default:
			return fmt.Errorf("join %s->%s: bad cardinality %q", j.From, j.To, j.Cardinality)
		}
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
