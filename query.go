package semantic

// Query is a semantic query: pure intent, zero table names, zero join keywords.
// "These metrics, by these dimensions, filtered like so, at this time grain."
type Query struct {
	Metrics    []string // metric names to compute
	GroupBy    []string // dimension names to slice by
	Where      []Filter // predicates on dimensions AND/OR on metrics (implicit AND between elements)
	TimeGrain  string   // day | week | month | quarter | year — applied to time dimensions in GroupBy
	OrderBy    string   // a metric or dimension name (presentation only)
	Descending bool
	Limit      int
	Offset     int

	// Roles are the caller's roles. A metric that declares `roles:` resolves
	// only when one of them is listed here. Empty Roles means "no role context"
	// — every role-restricted metric is refused, which is the safe default: a
	// caller that forgot to pass roles gets an error, not a leak.
	Roles []string
}

// Filter is one predicate, or a group of them.
//
// A LEAF filter names exactly one of Dimension or Metric:
//
//   - a dimension leaf restricts raw rows BEFORE aggregation (SQL WHERE, inside
//     each measure's CTE);
//   - a metric leaf restricts groups AFTER aggregation (SQL HAVING, applied to
//     the assembled outer query).
//
// A GROUP filter sets And or Or instead, nesting further filters. The two kinds
// cannot be mixed inside one group: pre- and post-aggregation predicates are not
// expressible in a single SQL clause, so `(region = 'x' OR revenue > 10)` is
// refused rather than silently compiled into something that means neither.
type Filter struct {
	Dimension string `json:"dimension,omitempty"`
	Metric    string `json:"metric,omitempty"`
	Op        string `json:"op,omitempty"`     // see filterOps
	Values    []any  `json:"values,omitempty"` // one value for scalar ops; many for in/not in; two for between

	And []Filter `json:"and,omitempty"`
	Or  []Filter `json:"or,omitempty"`
}

// IsGroup reports whether f nests other filters rather than naming a member.
func (f Filter) IsGroup() bool { return len(f.And) > 0 || len(f.Or) > 0 }

// filterKind classifies a filter subtree as dimension-only, metric-only, or
// mixed. Mixed is always an error; the caller reports it with context.
type filterKind int

const (
	kindEmpty filterKind = iota
	kindDimension
	kindMetric
	kindMixed
)

func mergeKind(a, b filterKind) filterKind {
	switch {
	case a == kindEmpty:
		return b
	case b == kindEmpty:
		return a
	case a == kindMixed || b == kindMixed || a != b:
		return kindMixed
	default:
		return a
	}
}

// kindOf walks a filter subtree and reports what it predicates on.
func kindOf(f Filter) filterKind {
	if f.IsGroup() {
		k := kindEmpty
		for _, sub := range append(append([]Filter{}, f.And...), f.Or...) {
			k = mergeKind(k, kindOf(sub))
		}
		return k
	}
	switch {
	case f.Metric != "":
		return kindMetric
	case f.Dimension != "":
		return kindDimension
	default:
		return kindEmpty
	}
}
