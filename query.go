package semantic

// Query is a semantic query: pure intent, zero table names, zero join keywords.
// "These metrics, by these dimensions, filtered like so, at this time grain."
type Query struct {
	Metrics    []string // metric names to compute
	GroupBy    []string // dimension names to slice by
	Where      []Filter // filters expressed against dimensions, not raw columns
	TimeGrain  string   // day | week | month | quarter | year — applied to time dimensions in GroupBy
	OrderBy    string   // a metric or dimension name (presentation only)
	Descending bool
	Limit      int
}

// Filter is one predicate against a dimension.
type Filter struct {
	Dimension string `json:"dimension"`
	Op        string `json:"op"`     // = | != | > | >= | < | <= | in
	Values    []any  `json:"values"` // one value for scalar ops; many for "in"
}
