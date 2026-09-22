package semantic

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Cube-shaped query JSON
//
// There is no cross-vendor standard for semantic-layer QUERIES — the
// interchange formats standardize the model and stop there — so this package
// speaks the JSON shape Cube's REST API uses, because it is the one most widely
// copied and therefore the one a language model is most likely to emit
// correctly without being taught.
//
// Adopting someone else's shape also buys a conformance oracle: the same JSON
// can be handed to Cube and the SQL it emits diffed against this compiler's.
//
// Members are namespaced `dataset.member`. Values are always strings, dates as
// YYYY-MM-DD. Time is carried in its own `timeDimensions` array rather than as
// an ordinary filter.
// ---------------------------------------------------------------------------

// CubeQuery is the wire shape. It is deliberately permissive about what it
// accepts and strict about what it converts: a field this layer cannot honour
// exactly is an error, never a silent drop.
type CubeQuery struct {
	Measures       []string            `json:"measures"`
	Dimensions     []string            `json:"dimensions"`
	Filters        []CubeFilter        `json:"filters"`
	TimeDimensions []CubeTimeDimension `json:"timeDimensions"`
	Segments       []string            `json:"segments"`
	Limit          int                 `json:"limit"`
	Offset         int                 `json:"offset"`
	Order          json.RawMessage     `json:"order"`
	Timezone       string              `json:"timezone"`

	// The spec this package was written against also shows filters split into
	// two arrays by clause. Both spellings mean the same thing here — the
	// member decides which clause a predicate lands in, not which array it
	// arrived in — and accepting both keeps a caller from having to know which
	// document it read.
	DimensionFilters []CubeFilter `json:"dimensionFilters"`
	MeasureFilters   []CubeFilter `json:"measureFilters"`
}

// CubeFilter is a leaf predicate or a boolean group.
type CubeFilter struct {
	Member   string   `json:"member"`
	Operator string   `json:"operator"`
	Values   []string `json:"values"`

	And []CubeFilter `json:"and"`
	Or  []CubeFilter `json:"or"`
}

// CubeTimeDimension carries a time filter and, optionally, the grain to bucket
// by. A time dimension with a granularity is grouped by; one without only
// filters — that asymmetry is Cube's, and it is load-bearing, so it is kept.
type CubeTimeDimension struct {
	Dimension   string          `json:"dimension"`
	DateRange   json.RawMessage `json:"dateRange"`
	Granularity string          `json:"granularity"`
}

// ParseCubeQuery decodes Cube-shaped JSON, rejecting unknown fields so a
// misspelled key is a loud error rather than a silently unapplied filter.
func ParseCubeQuery(data []byte) (CubeQuery, error) {
	var q CubeQuery
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&q); err != nil {
		return CubeQuery{}, fmt.Errorf("parse cube query: %w", err)
	}
	return q, nil
}

// ToQuery converts a Cube-shaped request into a semantic Query against this
// model, validating every member against the model as it goes. An unknown
// member, an operator this layer cannot express exactly, or a request feature
// that would have to be approximated is refused here — before any SQL exists.
func (m *Model) ToQuery(cq CubeQuery, roles []string) (Query, error) {
	var q Query
	q.Roles = roles
	q.Limit = cq.Limit
	q.Offset = cq.Offset

	if len(cq.Segments) > 0 {
		return q, fmt.Errorf("segments %v are not part of the semantic model: express the predicate as a dimension filter, "+
			"or add it to the model as a dimension", cq.Segments)
	}
	// A timezone we cannot honour is worse than one we reject: every day,
	// week and month boundary in the answer would be shifted, and nothing in
	// the result would say so.
	if tz := strings.TrimSpace(cq.Timezone); tz != "" && !strings.EqualFold(tz, "UTC") {
		return q, fmt.Errorf("timezone %q is not supported: this layer truncates dates in the engine's own zone, "+
			"so honouring it would require a conversion the model does not declare — pass UTC, or shift the dates yourself", tz)
	}

	if len(cq.Measures) == 0 {
		return q, fmt.Errorf("cube query has no measures")
	}
	for _, member := range cq.Measures {
		name, err := m.resolveMember(member, memberMetric)
		if err != nil {
			return q, err
		}
		q.Metrics = append(q.Metrics, name)
	}
	for _, member := range cq.Dimensions {
		name, err := m.resolveMember(member, memberDimension)
		if err != nil {
			return q, err
		}
		q.GroupBy = append(q.GroupBy, name)
	}

	// Time dimensions: the date range becomes an ordinary dimension filter; the
	// granularity becomes the query's time grain and puts the dimension in the
	// group-by.
	grain := ""
	for _, td := range cq.TimeDimensions {
		name, err := m.resolveMember(td.Dimension, memberDimension)
		if err != nil {
			return q, err
		}
		if d := m.Dimension(name); d != nil && d.Type != "time" {
			return q, fmt.Errorf("timeDimensions names %q, which the model declares as %s, not time", name, d.Type)
		}
		if td.Granularity != "" {
			g := strings.ToLower(td.Granularity)
			if !validPeriod(g) {
				return q, fmt.Errorf("granularity %q is not supported (day|week|month|quarter|year)", td.Granularity)
			}
			if grain != "" && grain != g {
				return q, fmt.Errorf("two time dimensions ask for different granularities (%q and %q): "+
					"this layer applies one grain per query", grain, g)
			}
			grain = g
			if !containsName(q.GroupBy, name) {
				q.GroupBy = append(q.GroupBy, name)
			}
		}
		f, err := dateRangeFilter(name, td.DateRange)
		if err != nil {
			return q, err
		}
		if f != nil {
			q.Where = append(q.Where, *f)
		}
	}
	q.TimeGrain = grain

	for _, group := range [][]CubeFilter{cq.Filters, cq.DimensionFilters, cq.MeasureFilters} {
		for _, cf := range group {
			f, err := m.convertFilter(cf)
			if err != nil {
				return q, err
			}
			q.Where = append(q.Where, f)
		}
	}

	if err := m.applyCubeOrder(&q, cq.Order); err != nil {
		return q, err
	}
	return q, nil
}

// CompileCube is the whole path in one call: Cube JSON in, grain-safe SQL out.
func CompileCube(m *Model, data []byte, roles []string, d Dialect) (Compiled, error) {
	cq, err := ParseCubeQuery(data)
	if err != nil {
		return Compiled{}, err
	}
	q, err := m.ToQuery(cq, roles)
	if err != nil {
		return Compiled{}, err
	}
	return Compile(m, q, d)
}

type memberKind int

const (
	memberAny memberKind = iota
	memberMetric
	memberDimension
)

// resolveMember strips the `dataset.` namespace Cube puts on every member and
// resolves what remains against the model. When a namespace is present it is
// checked, not discarded: `orders.total_revenue` naming a metric whose native
// dataset is order_item is a real mistake about where a number lives, and the
// point of this layer is to catch exactly that class of mistake early.
func (m *Model) resolveMember(member string, want memberKind) (string, error) {
	raw := strings.TrimSpace(member)
	if raw == "" {
		return "", fmt.Errorf("empty member name")
	}
	ns, local := "", raw
	if i := strings.LastIndex(raw, "."); i >= 0 {
		ns, local = raw[:i], raw[i+1:]
	}

	metricName, isMetric := m.ResolveMetricName(local)
	dimName, isDim := m.ResolveDimensionName(local)

	switch want {
	case memberMetric:
		isDim = false
	case memberDimension:
		isMetric = false
	}
	switch {
	case isMetric && isDim:
		return "", fmt.Errorf("member %q is ambiguous: it names both a metric and a dimension", member)
	case isMetric:
		if err := m.checkNamespace(ns, m.Metric(metricName).Entity, member, metricName); err != nil {
			return "", err
		}
		return metricName, nil
	case isDim:
		if err := m.checkNamespace(ns, m.Dimension(dimName).Entity, member, dimName); err != nil {
			return "", err
		}
		return dimName, nil
	}
	return "", m.unknownMember(member, local, want)
}

// checkNamespace verifies the `dataset.` prefix against where the member lives.
// A derived or window metric has no native dataset; any namespace is allowed on
// those, because there is no single right answer to check against.
func (m *Model) checkNamespace(ns, owner, member, local string) error {
	if ns == "" || owner == "" {
		return nil
	}
	// Accept the entity name or its physical table, and tolerate a fully
	// qualified source (db.schema.table) by comparing the last segment.
	last := ns
	if i := strings.LastIndex(ns, "."); i >= 0 {
		last = ns[i+1:]
	}
	if last == owner {
		return nil
	}
	if e := m.Entity(owner); e != nil && (last == e.Table || lastSegment(e.Table) == last) {
		return nil
	}
	return fmt.Errorf("member %q is namespaced to %q but %q belongs to dataset %q", member, ns, local, owner)
}

func lastSegment(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[i+1:]
	}
	return s
}

func (m *Model) unknownMember(member, local string, want memberKind) error {
	switch want {
	case memberMetric:
		if sugg := m.SuggestMetricNames(local, 3); len(sugg) > 0 {
			return fmt.Errorf("unknown measure %q; did you mean %s?", member, humanList(sugg))
		}
		return fmt.Errorf("unknown measure %q (known: %s)", member, summarizeNames(m.MetricNames()))
	case memberDimension:
		return fmt.Errorf("unknown dimension %q (known: %s)", member, summarizeNames(m.DimensionNames()))
	default:
		return fmt.Errorf("unknown member %q: it names neither a metric (%s) nor a dimension (%s)",
			member, summarizeNames(m.MetricNames()), summarizeNames(m.DimensionNames()))
	}
}

// convertFilter turns one Cube filter (leaf or group) into a semantic Filter.
func (m *Model) convertFilter(cf CubeFilter) (Filter, error) {
	if len(cf.And) > 0 || len(cf.Or) > 0 {
		if cf.Member != "" || cf.Operator != "" {
			return Filter{}, fmt.Errorf("filter on %q sets both a member and an and/or group", cf.Member)
		}
		var out Filter
		for _, sub := range cf.And {
			f, err := m.convertFilter(sub)
			if err != nil {
				return Filter{}, err
			}
			out.And = append(out.And, f)
		}
		for _, sub := range cf.Or {
			f, err := m.convertFilter(sub)
			if err != nil {
				return Filter{}, err
			}
			out.Or = append(out.Or, f)
		}
		// The mixed-clause rule is enforced here too, so a caller building
		// Cube JSON learns about it at conversion time with its own member
		// names in hand, rather than from the compiler two layers down.
		if kindOf(out) == kindMixed {
			return Filter{}, fmt.Errorf("filter group mixes dimension and measure members: " +
				"dimension filters become WHERE (before aggregation) and measure filters become HAVING (after it), " +
				"so one and/or group cannot hold both")
		}
		return out, nil
	}

	name, err := m.resolveMember(cf.Member, memberAny)
	if err != nil {
		return Filter{}, err
	}
	op, values, err := cubeOperator(cf.Operator, cf.Values)
	if err != nil {
		return Filter{}, fmt.Errorf("filter on %q: %w", cf.Member, err)
	}
	f := Filter{Op: op, Values: values}
	if _, isMetric := m.ResolveMetricName(name); isMetric {
		f.Metric = name
		// Cube's wire convention is that every value is a string. For a measure
		// that convention is only a transport detail — a measure is a number —
		// and binding "50000" as text against a numeric aggregate is engine
		// roulette: some coerce, some compare lexically (where "9" > "50000"),
		// some error. Convert here, and refuse a value that is not a number
		// rather than let one of those three happen.
		nums, err := numericValues(values)
		if err != nil {
			return Filter{}, fmt.Errorf("filter on measure %q: %w", cf.Member, err)
		}
		f.Values = nums
	} else {
		f.Dimension = name
	}
	return f, nil
}

// numericValues parses measure-filter values as numbers, preserving integers as
// integers so a count compares against a whole number rather than a float.
func numericValues(values []any) ([]any, error) {
	out := make([]any, len(values))
	for i, v := range values {
		s := strings.TrimSpace(fmt.Sprint(v))
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			out[i] = n
			continue
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number (a measure filter compares against a number)", s)
		}
		out[i] = f
	}
	return out, nil
}

// cubeOperator maps a Cube operator onto this layer's predicate vocabulary.
// Every operator is translated exactly or refused: an operator quietly mapped
// to its nearest neighbour is a filter that does not mean what it says.
func cubeOperator(op string, values []string) (string, []any, error) {
	vals := make([]any, len(values))
	for i, v := range values {
		vals[i] = v
	}
	need := func(n int) error {
		if len(vals) < n {
			return fmt.Errorf("operator %q needs %d value(s), got %d", op, n, len(vals))
		}
		return nil
	}
	switch op {
	case "equals":
		if err := need(1); err != nil {
			return "", nil, err
		}
		if len(vals) == 1 {
			return "=", vals, nil
		}
		return "in", vals, nil
	case "notEquals":
		if err := need(1); err != nil {
			return "", nil, err
		}
		if len(vals) == 1 {
			return "!=", vals, nil
		}
		return "not in", vals, nil
	case "contains":
		return "contains", vals, need(1)
	case "notContains":
		return "not contains", vals, need(1)
	case "startsWith":
		return "starts with", vals, need(1)
	case "endsWith":
		return "ends with", vals, need(1)
	case "gt":
		return ">", vals, need(1)
	case "gte":
		return ">=", vals, need(1)
	case "lt":
		return "<", vals, need(1)
	case "lte":
		return "<=", vals, need(1)
	case "set":
		return "is not null", nil, nil
	case "notSet":
		return "is null", nil, nil
	case "inDateRange":
		return "between", vals, need(2)
	case "notInDateRange":
		return "not between", vals, need(2)
	case "beforeDate":
		return "<", vals, need(1)
	case "afterDate":
		return ">", vals, need(1)
	case "":
		return "", nil, fmt.Errorf("missing operator")
	default:
		return "", nil, fmt.Errorf("unsupported operator %q", op)
	}
}

// dateRangeFilter turns a timeDimensions dateRange into a BETWEEN filter.
// An absolute [from, to] pair converts; a relative range ("last 7 days") is
// refused, because resolving it needs a clock and a timezone that the semantic
// model does not carry — and a date window silently resolved against the wrong
// "now" produces an answer that is wrong in a way no reader can see.
func dateRangeFilter(dimension string, raw json.RawMessage) (*Filter, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var pair []string
	if err := json.Unmarshal(raw, &pair); err == nil {
		if len(pair) != 2 {
			return nil, fmt.Errorf("dateRange on %q needs exactly two dates, got %d", dimension, len(pair))
		}
		return &Filter{Dimension: dimension, Op: "between", Values: []any{pair[0], pair[1]}}, nil
	}
	var rel string
	if err := json.Unmarshal(raw, &rel); err == nil {
		return nil, fmt.Errorf("relative dateRange %q on %q is not supported: this layer has no clock or timezone, "+
			"so resolve it to absolute dates [\"YYYY-MM-DD\", \"YYYY-MM-DD\"] before compiling", rel, dimension)
	}
	return nil, fmt.Errorf("dateRange on %q must be [\"YYYY-MM-DD\", \"YYYY-MM-DD\"]", dimension)
}

// applyCubeOrder accepts Cube's array form [["member","desc"]] and its object
// form {"member":"desc"}. More than one sort key is refused: this layer emits a
// single ORDER BY, and honouring only the first key of several would reorder
// the result in a way the request did not ask for.
func (m *Model) applyCubeOrder(q *Query, raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	type kv struct {
		member string
		dir    string
	}
	var keys []kv

	var arr [][]string
	if err := json.Unmarshal(raw, &arr); err == nil {
		for _, pair := range arr {
			if len(pair) == 0 {
				continue
			}
			dir := "asc"
			if len(pair) > 1 {
				dir = pair[1]
			}
			keys = append(keys, kv{pair[0], dir})
		}
	} else {
		var obj map[string]string
		if err := json.Unmarshal(raw, &obj); err != nil {
			return fmt.Errorf("order must be {\"member\":\"asc|desc\"} or [[\"member\",\"asc|desc\"]]")
		}
		names := make([]string, 0, len(obj))
		for k := range obj {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			keys = append(keys, kv{k, obj[k]})
		}
	}
	switch len(keys) {
	case 0:
		return nil
	case 1:
	default:
		return fmt.Errorf("order names %d sort keys; this layer emits a single ORDER BY", len(keys))
	}
	name, err := m.resolveMember(keys[0].member, memberAny)
	if err != nil {
		return err
	}
	switch strings.ToLower(keys[0].dir) {
	case "", "asc":
		q.Descending = false
	case "desc":
		q.Descending = true
	default:
		return fmt.Errorf("order direction %q must be asc or desc", keys[0].dir)
	}
	q.OrderBy = name
	return nil
}
