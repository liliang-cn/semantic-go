package semantic

import (
	"fmt"
	"regexp"
	"strings"
)

// Compiled is the output: SQL, ordered bind arguments, and the provenance chain
// that says which metrics, dimensions and declared joins produced them.
type Compiled struct {
	SQL        string
	Args       []any
	Provenance Provenance
}

// Provenance is the answer's paper trail: every name the compiler resolved and
// every edge it traversed. An answer that cannot say which metric definition it
// came from is an answer nobody can check.
type Provenance struct {
	Dialect string `json:"dialect"`

	// Metrics is one entry per projected metric, in request order.
	Metrics []MetricUse `json:"metrics"`
	// FilterMetrics is one entry per metric used only to filter (never shown).
	FilterMetrics []MetricUse `json:"filter_metrics,omitempty"`
	// Dimensions is the group-by, canonicalized.
	Dimensions []string `json:"dimensions,omitempty"`
	// TimeGrain is the grain applied to time dimensions, if any.
	TimeGrain string `json:"time_grain,omitempty"`
	// Joins lists each declared edge traversed, as "from.key → to.key".
	Joins []string `json:"joins,omitempty"`
	// Entities is every dataset the query touched, in traversal order.
	Entities []string `json:"entities,omitempty"`
}

// MetricUse records one metric as the compiler resolved it.
type MetricUse struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Kind        string   `json:"kind"` // base | derived | window
	Entity      string   `json:"entity,omitempty"`
	Agg         string   `json:"agg,omitempty"`
	Expr        string   `json:"expr,omitempty"`
	Formula     string   `json:"formula,omitempty"`
	Window      string   `json:"window,omitempty"`
	Additivity  string   `json:"additivity"`
	BaseMetrics []string `json:"base_metrics,omitempty"`
}

// Compile turns a semantic Query into fanout/chasm-safe SQL for the dialect.
//
// Algorithm: each base metric is aggregated to the requested dimension grain in
// its OWN CTE (joining only upward via many-to-one edges, so the leaf grain is
// never multiplied); the CTEs are then null-safe outer-joined on the shared
// dimensions and combined (derived formulas computed in the outer SELECT). This
// is what makes fan-out and chasm joins impossible by construction.
//
// A metric named only in a filter is compiled the same way — its own CTE at its
// own native grain — and then LEFT JOINed onto the projected measures' spine and
// tested in the outer WHERE. That is how "night-shift hours by region, for
// regions whose total cost exceeded 50000" compiles when hours and cost live in
// different fact tables at different grains: aggregate each where it lives, join
// up to the conformed dimension, filter after.
func Compile(m *Model, q Query, d Dialect) (Compiled, error) {
	if len(q.Metrics) == 0 {
		return Compiled{}, fmt.Errorf("query has no metrics")
	}
	c := &compiler{m: m, q: q, d: d, baseSeen: map[string]bool{}, entitySeen: map[string]bool{}}

	// 1) Split the filter tree into pre-aggregation (dimension) and
	//    post-aggregation (metric) halves, refusing any group that mixes them.
	if err := c.splitFilters(); err != nil {
		return Compiled{}, err
	}

	// 2) Role gate, before anything else resolves: a restricted metric must not
	//    even reveal its shape through an error about its formula.
	for _, name := range q.Metrics {
		if err := m.checkRoles(name, q.Roles, map[string]bool{}); err != nil {
			return Compiled{}, err
		}
	}
	for _, name := range c.filterMetrics {
		if err := m.checkRoles(name, q.Roles, map[string]bool{}); err != nil {
			return Compiled{}, err
		}
	}

	// 3) A repeated name would put two identically-named columns in the result,
	//    which is ambiguous for whatever reads it back and useless either way.
	if dup := firstDuplicate(q.Metrics); dup != "" {
		return Compiled{}, fmt.Errorf("metric %q is requested twice: the result would carry two columns under that name", dup)
	}
	if dup := firstDuplicate(q.GroupBy); dup != "" {
		return Compiled{}, fmt.Errorf("dimension %q is grouped by twice: the result would carry two columns under that name", dup)
	}

	// 4) Resolve group-by dimensions (needed by window metrics), and refuse a
	//    filter on a dimension this caller is not allowed to see.
	if err := c.resolveDims(q.GroupBy); err != nil {
		return Compiled{}, err
	}
	if err := c.checkFilterVisibility(); err != nil {
		return Compiled{}, err
	}

	// 2b) An offset with no limit is refused. Every engine spells LIMIT
	//    differently and several reject OFFSET on its own, so honouring it would
	//    mean inventing a maximum — MySQL's 18446744073709551615, SQLite's -1 —
	//    per dialect, for a query nobody writes: pagination always carries a
	//    page size. Refusing is one line; three engine-specific sentinels that
	//    nothing here can execute against is a standing liability.
	if q.Offset > 0 && q.Limit <= 0 {
		return Compiled{}, fmt.Errorf("offset %d with no limit: set a limit (a page offset without a page size is not portable across engines)", q.Offset)
	}

	// 5) ORDER BY names an output column, so it has to be one. Left unchecked
	//    it reached the warehouse as an unresolvable identifier — an error, but
	//    one raised by the engine about generated SQL rather than here about
	//    the request.
	if q.OrderBy != "" && !containsName(q.Metrics, q.OrderBy) && !containsName(q.GroupBy, q.OrderBy) {
		return Compiled{}, fmt.Errorf("order_by %q is neither a requested metric nor a group-by dimension "+
			"(available: %s)", q.OrderBy, strings.Join(append(append([]string{}, q.GroupBy...), q.Metrics...), ", "))
	}

	// 6) A measure filter with nothing to group by is a HAVING on a single
	//    grand total. It is ambiguous rather than unsafe — "revenue > 1000"
	//    over one row either returns that row or nothing, which is a question
	//    about a threshold, not a question about data — so refuse it rather
	//    than let the behaviour emerge from whichever branch happens to run.
	if len(c.metricFilters) > 0 && len(c.dims) == 0 {
		return Compiled{}, fmt.Errorf(
			"measure filter on %s with no group_by: a HAVING on a single grand total is ambiguous — "+
				"add a group_by, or filter the rows with a dimension filter instead",
			humanList(c.filterMetrics))
	}

	// 7) Pass 1 — classify: collect the base metrics every requested metric
	//    depends on (through derived formulas and window `of`), without emitting
	//    SQL. Projected metrics first: the leading run of baseOrder is what the
	//    dimension spine is built from, so a filter-only measure can never add
	//    rows to the result.
	for _, name := range q.Metrics {
		if err := c.collectBases(name, map[string]bool{}); err != nil {
			return Compiled{}, err
		}
	}
	if len(c.baseOrder) == 0 {
		return Compiled{}, fmt.Errorf("query resolves to no base measure (a formula that references no metric cannot be aggregated)")
	}
	c.nProjected = len(c.baseOrder)
	for _, name := range c.filterMetrics {
		if err := c.collectBases(name, map[string]bool{}); err != nil {
			return Compiled{}, err
		}
	}
	c.single = len(c.baseOrder) == 1

	// 8) Pass 2 — build the outer-SELECT expression for each requested metric.
	outCols := make([]string, 0, len(q.Metrics))
	for _, name := range q.Metrics {
		expr, err := c.exprFor(name, map[string]bool{})
		if err != nil {
			return Compiled{}, err
		}
		outCols = append(outCols, expr+" AS "+d.QuoteIdent(name))
	}

	// 9) The semi-additive gate: a level may be summed across anything except
	//    time, and every base measure this query touches is checked, including
	//    the ones reached through a formula and the ones used only to filter.
	for _, bm := range c.baseOrder {
		if err := c.checkSemiAdditive(bm); err != nil {
			return Compiled{}, err
		}
	}

	// 10) Build one CTE per base metric (aggregate-to-grain-first).
	var ctes []string
	for _, bm := range c.baseOrder {
		cte, err := c.buildCTE(bm, c.dims)
		if err != nil {
			return Compiled{}, err
		}
		ctes = append(ctes, cte)
	}

	// 11) Build the post-aggregation predicates AFTER every CTE, so the bind
	//    arguments stay in the order the placeholders appear in the SQL text.
	//    (Positional dialects care; ?-dialects do not, and paying the same
	//    discipline for both is cheaper than remembering which is which.)
	if err := c.buildHaving(); err != nil {
		return Compiled{}, err
	}

	// 12) Assemble the outer query joining the CTEs on the dimensions.
	sql := c.assemble(ctes, outCols)
	return Compiled{SQL: sql, Args: c.args, Provenance: c.provenance()}, nil
}

type compiler struct {
	m          *Model
	q          Query
	d          Dialect
	args       []any
	baseOrder  []string // base metrics in first-seen order; [:nProjected] are projected
	nProjected int
	baseSeen   map[string]bool
	dims       []resolvedDim
	single     bool

	dimFilters    []Filter // pre-aggregation subtrees (dimension leaves only)
	metricFilters []Filter // post-aggregation subtrees (metric leaves only)
	filterMetrics []string // metric names named anywhere in metricFilters
	havingPreds   []string // compiled post-aggregation predicates

	joinsUsed  []string // declared edges traversed, for provenance
	entityUsed []string
	entitySeen map[string]bool
}

// splitFilters walks q.Where once: every top-level element must predicate on
// dimensions only or on metrics only, and the same holds inside every nested
// and/or group. A group that mixes the two is refused, because "restrict rows
// before aggregating" and "restrict groups after aggregating" are different SQL
// clauses and no single clause expresses their disjunction.
func (c *compiler) splitFilters() error {
	seenMetric := map[string]bool{}
	for _, f := range c.q.Where {
		if err := c.m.validateFilter(f); err != nil {
			return err
		}
		switch kindOf(f) {
		case kindEmpty:
			return fmt.Errorf("filter names neither a dimension nor a metric: %+v", f)
		case kindMixed:
			return fmt.Errorf("a filter group mixes dimension and measure predicates — " +
				"dimension filters restrict rows before aggregation (WHERE) and measure filters restrict groups " +
				"after it (HAVING); they cannot share an and/or group. Split them into separate top-level filters")
		case kindDimension:
			c.dimFilters = append(c.dimFilters, f)
		case kindMetric:
			c.metricFilters = append(c.metricFilters, f)
			collectFilterMetrics(f, seenMetric, &c.filterMetrics)
		}
	}
	return nil
}

func collectFilterMetrics(f Filter, seen map[string]bool, out *[]string) {
	if f.IsGroup() {
		for _, sub := range f.And {
			collectFilterMetrics(sub, seen, out)
		}
		for _, sub := range f.Or {
			collectFilterMetrics(sub, seen, out)
		}
		return
	}
	if f.Metric != "" && !seen[f.Metric] {
		seen[f.Metric] = true
		*out = append(*out, f.Metric)
	}
}

// cte names the per-metric CTE, and keysAlias the UNION-ed key spine. Both are
// generated identifiers, so both go through the dialect's quoting: "keys" is a
// reserved word in MySQL, and an unquoted one turned every multi-metric query
// into a syntax error. Quoting a generated name costs nothing and removes a
// whole class of collision with whatever each engine happens to reserve.
func (c *compiler) cte(metric string) string { return c.d.QuoteIdent("m_" + metric) }

func (c *compiler) keysAlias() string { return c.d.QuoteIdent("keys") }

// dimRef is how a dimension column is referenced in the outer SELECT (and in
// window OVER clauses): from the single base CTE, or the UNION-ed `keys` spine.
func (c *compiler) dimRef(d resolvedDim) string {
	if c.single {
		return c.cte(c.baseOrder[0]) + "." + c.d.QuoteIdent(d.name)
	}
	return c.keysAlias() + "." + c.d.QuoteIdent(d.name)
}

func (c *compiler) ph(v any) string {
	c.args = append(c.args, v)
	return c.d.Placeholder(len(c.args))
}

func (c *compiler) useBase(name string) {
	if !c.baseSeen[name] {
		c.baseSeen[name] = true
		c.baseOrder = append(c.baseOrder, name)
	}
}

func (c *compiler) useEntity(name string) {
	if !c.entitySeen[name] {
		c.entitySeen[name] = true
		c.entityUsed = append(c.entityUsed, name)
	}
}

var identRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// collectBases records every base (simple-aggregation) metric a requested metric
// depends on, recursing through derived formulas and window `of`.
func (c *compiler) collectBases(name string, visiting map[string]bool) error {
	mt := c.m.Metric(name)
	if mt == nil {
		return c.unknownMetric(name)
	}
	if visiting[name] {
		return fmt.Errorf("metric %q has a cyclic definition", name)
	}
	visiting[name] = true
	defer delete(visiting, name)

	switch {
	case mt.IsWindow():
		if mt.Of == "" {
			return fmt.Errorf("window metric %q needs `of`", name)
		}
		return c.collectBases(mt.Of, visiting)
	case mt.IsDerived():
		var rerr error
		identRe.ReplaceAllStringFunc(mt.Formula, func(tok string) string {
			if c.m.Metric(tok) != nil && rerr == nil {
				rerr = c.collectBases(tok, visiting)
			}
			return tok
		})
		return rerr
	default:
		c.useBase(name)
		return nil
	}
}

// unknownMetric reports a missing metric with the closest known names, hiding
// any the caller's roles do not admit.
func (c *compiler) unknownMetric(name string) error {
	visible := c.m.VisibleMetricNames(c.q.Roles)
	if sugg := c.m.SuggestMetricNames(name, 3); len(sugg) > 0 {
		var keep []string
		for _, s := range sugg {
			for _, v := range visible {
				if s == v {
					keep = append(keep, s)
					break
				}
			}
		}
		if len(keep) > 0 {
			return fmt.Errorf("unknown metric %q; did you mean %s?", name, humanList(keep))
		}
	}
	return fmt.Errorf("unknown metric %q (known: %s)", name, summarizeNames(visible))
}

// exprFor returns the outer-SELECT SQL for a requested metric, with missing
// rows read as zero.
func (c *compiler) exprFor(name string, visiting map[string]bool) (string, error) {
	return c.exprForMode(name, visiting, true)
}

// exprForMode builds a metric's outer expression. coalesce decides how "this
// group has no rows in this measure's CTE" reads.
//
// Projecting (coalesce=true): a missing row is 0, which is what a sum of no
// rows means and what a reader expects in a table cell.
//
// Filtering (coalesce=false): a missing row stays NULL, so the predicate is
// unknown and the group drops out. This is the conservative reading and it is
// chosen deliberately: coalescing to 0 would make "cost < 1000" true for every
// region that has no cost data at all, turning absence of evidence into
// evidence. A group is kept only when the filtering measure actually has a
// value there.
func (c *compiler) exprForMode(name string, visiting map[string]bool, coalesce bool) (string, error) {
	mt := c.m.Metric(name)
	if mt == nil {
		return "", c.unknownMetric(name)
	}
	switch {
	case mt.IsWindow():
		if !coalesce {
			return "", fmt.Errorf("measure filter on window metric %q is not supported: a window function cannot appear in a WHERE/HAVING clause "+
				"(filter the base metric %q instead, or wrap the query yourself)", name, mt.Of)
		}
		return c.windowExpr(mt)
	case mt.IsDerived():
		if visiting[name] {
			return "", fmt.Errorf("derived metric %q has a cyclic formula", name)
		}
		// Lint reports this at build time with a suggestion; the compiler
		// repeats it because a caller may have skipped the gate, and the
		// alternative is emitting the typo as a bare column reference.
		if bad := c.m.unknownFormulaRefs(mt.Formula); len(bad) > 0 {
			return "", fmt.Errorf("metric %q: formula references %s, which name no metric", name, humanList(bad))
		}
		visiting[name] = true
		defer delete(visiting, name)
		var rerr error
		out := identRe.ReplaceAllStringFunc(mt.Formula, func(tok string) string {
			if c.m.Metric(tok) == nil {
				return tok // SQL function/literal
			}
			sub, err := c.exprForMode(tok, visiting, coalesce)
			if err != nil {
				rerr = err
				return tok
			}
			// Cast at the point of substitution, not at the metric's own
			// definition: a formula is where the division happens, and where a
			// count of integers stops being a count and becomes a numerator.
			// A metric selected on its own keeps its natural type.
			return c.d.CastDecimal(sub)
		})
		if rerr != nil {
			return "", rerr
		}
		return "(" + out + ")", nil
	default:
		ref := c.cte(name) + "." + c.d.QuoteIdent(name)
		if coalesce {
			return fmt.Sprintf("COALESCE(%s, 0)", ref), nil
		}
		return ref, nil
	}
}

var windowRe = regexp.MustCompile(`^(rolling|prior|delta):(\d+)$`)

// windowExpr emits a window function over the base metric `of`, ordered by the
// (single) time dimension in group-by and partitioned by the other dimensions.
func (c *compiler) windowExpr(mt *Metric) (string, error) {
	value, err := c.exprFor(mt.Of, map[string]bool{})
	if err != nil {
		return "", err
	}
	var timeRef string
	var parts []string
	for _, d := range c.dims {
		if d.typ == "time" && timeRef == "" {
			timeRef = c.dimRef(d)
		} else {
			parts = append(parts, c.dimRef(d))
		}
	}
	if timeRef == "" {
		return "", fmt.Errorf("window metric %q needs a time dimension in group_by", mt.Name)
	}
	// Grain-to-date: restart the window at each boundary of mt.Reset (e.g.
	// reset: year → YTD) by partitioning on the truncated period. Applying
	// date_trunc to the already-grain-truncated timeRef is exact, since
	// date_trunc(year, date_trunc(month, x)) = date_trunc(year, x).
	if mt.Reset != "" {
		parts = append(parts, c.d.DateTrunc(mt.Reset, timeRef))
	}
	over := "OVER ("
	if len(parts) > 0 {
		over += "PARTITION BY " + strings.Join(parts, ", ") + " "
	}
	over += "ORDER BY " + timeRef

	// rolling/cumulative re-sum `value` across rows: refuse if the underlying
	// measure is not safe to sum (semi/non-additive) — a structural guard
	// against a clean-running but wrong roll-up.
	isSum := mt.Window == "cumulative" || strings.HasPrefix(mt.Window, "rolling:")
	if isSum {
		if add := c.m.Additivity(mt.Of); add != Additive {
			return "", fmt.Errorf("window metric %q sums %s metric %q over time — refused (%s measures cannot be summed; use a point-in-time pick or a ratio metric)", mt.Name, add, mt.Of, add)
		}
	}

	switch {
	case mt.Window == "cumulative":
		return fmt.Sprintf("SUM(%s) %s ROWS UNBOUNDED PRECEDING)", value, over), nil
	}
	mtc := windowRe.FindStringSubmatch(mt.Window)
	if mtc == nil {
		return "", fmt.Errorf("metric %q: bad window %q (use rolling:N|cumulative|prior:N|delta:N)", mt.Name, mt.Window)
	}
	kind, n := mtc[1], mtc[2]
	switch kind {
	case "rolling":
		return fmt.Sprintf("SUM(%s) %s ROWS BETWEEN %s PRECEDING AND CURRENT ROW)", value, over, prevN(n)), nil
	case "prior":
		return fmt.Sprintf("LAG(%s, %s) %s)", value, n, over), nil
	case "delta":
		return fmt.Sprintf("(%s - LAG(%s, %s) %s))", value, value, n, over), nil
	}
	return "", fmt.Errorf("metric %q: unsupported window %q", mt.Name, mt.Window)
}

// prevN turns "3" into "2" (rolling:N spans N rows = N-1 preceding + current).
func prevN(n string) string {
	var x int
	fmt.Sscanf(n, "%d", &x)
	if x < 1 {
		x = 1
	}
	return fmt.Sprintf("%d", x-1)
}

type resolvedDim struct {
	name   string
	entity string
	typ    string // categorical | time
	sqlRaw string // qualified column, e.g. "stores"."region"
	sql    string // sqlRaw, or date_trunc(grain, sqlRaw) for time dims
}

func (c *compiler) resolveDims(names []string) error {
	out := make([]resolvedDim, 0, len(names))
	applied := false
	for _, n := range names {
		dim := c.m.Dimension(n)
		if dim == nil {
			return c.unknownDimension(n)
		}
		raw := c.qualify(dim.Entity, dim.Column)
		if !dim.visibleTo(c.q.Roles) {
			// The mask replaces the column everywhere it is projected AND
			// everywhere it is grouped, so the groups are the masked values
			// themselves. Grouping by the raw column and masking only the
			// output would leak the distinctness of the hidden values — one
			// row per real customer, all of them labelled '***'.
			// The mask is authored against the dimension's own dataset, so its
			// bare columns are qualified the same way a metric's expression is
			// — otherwise "substr(name, 1, 1)" breaks the day a joined table
			// also has a name column, and breaks at run time.
			raw = qualifyExpr(dim.Mask, dim.Entity, c.d)
		}
		expr := raw
		if dim.Type == "time" && c.q.TimeGrain != "" {
			expr = c.d.DateTrunc(c.q.TimeGrain, raw)
			applied = true
		}
		out = append(out, resolvedDim{name: n, entity: dim.Entity, typ: dim.Type, sqlRaw: raw, sql: expr})
	}
	// A grain that matched no time dimension used to be dropped in silence: ask
	// for revenue by month against a dimension the model calls categorical and
	// you got daily rows, correctly computed and not what you asked for. Nothing
	// about the answer says so. Refuse instead — a caller can act on an error.
	if c.q.TimeGrain != "" && !applied {
		return fmt.Errorf("time grain %q was requested but none of the group-by dimensions %v is declared type: time — "+
			"fix the dimension's type in the model, or drop the grain", c.q.TimeGrain, names)
	}
	c.dims = out
	return nil
}

func (c *compiler) unknownDimension(n string) error {
	return fmt.Errorf("unknown dimension %q (known: %s)", n, summarizeNames(c.m.DimensionNames()))
}

// qualify references a column by its entity ALIAS (the entity name), not the raw
// table. Aliasing every entity lets the same physical table appear more than once
// — role-playing dimensions (order_date vs ship_date) and bridge tables — without
// the join referencing an ambiguous table name.
func (c *compiler) qualify(entity, col string) string {
	return c.d.QuoteIdent(entity) + "." + c.d.QuoteIdent(col)
}

// buildCTE aggregates one base metric to the requested dimension grain.
func (c *compiler) buildCTE(metricName string, dims []resolvedDim) (string, error) {
	mt := c.m.Metric(metricName)
	base := mt.Entity

	// entities needed = base + dim entities + filter-dim entities
	need := map[string]bool{base: true}
	for _, d := range dims {
		need[d.entity] = true
	}
	var ferr error
	for _, f := range c.dimFilters {
		walkLeaves(f, func(leaf Filter) {
			fd := c.m.Dimension(leaf.Dimension)
			if fd == nil {
				if ferr == nil {
					ferr = c.unknownDimension(leaf.Dimension)
				}
				return
			}
			need[fd.Entity] = true
		})
	}
	if ferr != nil {
		return "", ferr
	}
	// The grain gate, enforced independently of describe_grain: a dataset whose
	// declared key tuple failed its uniqueness test is not aggregated over, and
	// is not joined through either — a duplicated row on the one-side of a join
	// fans out just as surely as an undeclared one-to-many edge.
	for ent := range need {
		if e := c.m.Entity(ent); e != nil && e.GrainStatus == GrainFail {
			return "", fmt.Errorf("metric %q needs dataset %q, whose grain test failed ((%s) is not unique in %s) — "+
				"the dataset is withheld from the semantic layer until it is fixed",
				metricName, ent, e.PrimaryKey, e.Table)
		}
	}
	joins, err := c.planJoins(base, need)
	if err != nil {
		return "", err
	}

	agg, err := aggExpr(mt.Agg, qualifyExpr(mt.Expr, base, c.d))
	if err != nil {
		return "", fmt.Errorf("metric %q: %w", metricName, err)
	}

	var sel []string
	var grp []string
	for _, d := range dims {
		sel = append(sel, d.sql+" AS "+c.d.QuoteIdent(d.name))
		grp = append(grp, d.sql)
	}
	sel = append(sel, agg+" AS "+c.d.QuoteIdent(metricName))

	var b strings.Builder
	// Alias the base table by entity name (e.g. FROM "orders" AS "order") so
	// columns qualify against the alias and role-playing/bridge entities on the
	// same physical table stay distinct.
	fmt.Fprintf(&b, "%s AS (\n  SELECT %s\n  FROM %s AS %s", c.cte(metricName), strings.Join(sel, ", "),
		c.d.QuoteIdent(c.m.Entity(base).Table), c.d.QuoteIdent(base))
	c.useEntity(base)
	for _, j := range joins {
		fmt.Fprintf(&b, "\n  JOIN %s AS %s ON %s",
			c.d.QuoteIdent(j.rightTable), c.d.QuoteIdent(j.rightAlias), c.on(j))
		c.useEntity(j.rightAlias)
		c.noteJoin(j)
	}
	if where := c.buildWhere(); where != "" {
		b.WriteString("\n  WHERE " + where)
	}
	if len(grp) > 0 {
		b.WriteString("\n  GROUP BY " + strings.Join(grp, ", "))
	}
	b.WriteString("\n)")
	return b.String(), nil
}

func (c *compiler) noteJoin(j joinStep) {
	edge := fmt.Sprintf("%s.%s → %s.%s",
		j.leftAlias, strings.Join(j.leftCols, "+"), j.rightAlias, strings.Join(j.rightCols, "+"))
	for _, e := range c.joinsUsed {
		if e == edge {
			return
		}
	}
	c.joinsUsed = append(c.joinsUsed, edge)
}

// walkLeaves visits every leaf predicate of a filter subtree.
func walkLeaves(f Filter, fn func(Filter)) {
	if f.IsGroup() {
		for _, sub := range f.And {
			walkLeaves(sub, fn)
		}
		for _, sub := range f.Or {
			walkLeaves(sub, fn)
		}
		return
	}
	fn(f)
}

// buildWhere compiles the pre-aggregation filter tree. It runs once per CTE (so
// every measure is filtered identically at its own grain), appending its bind
// arguments each time — which is why the placeholders are numbered, not shared.
func (c *compiler) buildWhere() string {
	var preds []string
	for _, f := range c.dimFilters {
		if s := c.filterSQL(f); s != "" {
			preds = append(preds, s)
		}
	}
	return strings.Join(preds, " AND ")
}

// filterSQL renders one dimension filter subtree.
func (c *compiler) filterSQL(f Filter) string {
	if f.IsGroup() {
		return c.groupSQL(f, func(sub Filter) string { return c.filterSQL(sub) })
	}
	fd := c.m.Dimension(f.Dimension)
	if fd == nil {
		return "" // unreachable: buildCTE validated names first
	}
	return c.predicate(c.qualify(fd.Entity, fd.Column), f)
}

// checkFilterVisibility refuses a filter on a dimension the caller may not see.
//
// Masking the projection while still allowing the predicate would turn the
// filter into an oracle: ask for revenue where email starts with 'a', then 'b',
// and the row counts read out the value the mask exists to hide. A masked
// dimension is therefore not filterable by a caller who cannot see it —
// refused, and told which role would grant it.
func (c *compiler) checkFilterVisibility() error {
	var err error
	for _, f := range c.dimFilters {
		walkLeaves(f, func(leaf Filter) {
			if err != nil {
				return
			}
			d := c.m.Dimension(leaf.Dimension)
			if d == nil || d.visibleTo(c.q.Roles) {
				return
			}
			err = fmt.Errorf("dimension %q is masked for this caller and cannot be filtered on: "+
				"a predicate over a hidden value reads it back one comparison at a time. "+
				"It remains available to group by, where it shows as %s. Roles that see it raw: %v",
				leaf.Dimension, d.Mask, d.Roles)
		})
	}
	return err
}

// groupSQL renders an and/or group by delegating each child to render.
func (c *compiler) groupSQL(f Filter, render func(Filter) string) string {
	join := func(subs []Filter, op string) string {
		var parts []string
		for _, sub := range subs {
			if s := render(sub); s != "" {
				parts = append(parts, s)
			}
		}
		if len(parts) == 0 {
			return ""
		}
		if len(parts) == 1 {
			return parts[0]
		}
		return "(" + strings.Join(parts, " "+op+" ") + ")"
	}
	var out []string
	if s := join(f.And, "AND"); s != "" {
		out = append(out, s)
	}
	if s := join(f.Or, "OR"); s != "" {
		out = append(out, s)
	}
	if len(out) == 1 {
		return out[0]
	}
	if len(out) == 0 {
		return ""
	}
	return "(" + strings.Join(out, " AND ") + ")"
}

// predicate renders one leaf against an already-qualified SQL expression.
//
// Every branch produces SQL. There is no fall-through returning "" — that is
// how an operator-less filter came to compile into nothing and hand back
// unfiltered data. validateFilter has already refused anything unknown, so
// reaching the default here is a bug in this package, and it says so rather
// than quietly matching every row.
func (c *compiler) predicate(col string, f Filter) string {
	switch normalizeOp(f.Op) {
	case "in":
		return col + " IN (" + c.phList(f.Values) + ")"
	case "not in":
		// NOT IN with a NULL in the list swallows every row; values are checked
		// for NULL up front, but the null-safe spelling costs one clause and
		// removes the question.
		return "(" + col + " NOT IN (" + c.phList(f.Values) + ") OR " + col + " IS NULL)"
	case "=", "!=", ">", ">=", "<", "<=":
		return col + " " + normalizeOp(f.Op) + " " + c.ph(f.Values[0])
	case "between":
		return col + " BETWEEN " + c.ph(f.Values[0]) + " AND " + c.ph(f.Values[1])
	case "not between":
		return "(" + col + " NOT BETWEEN " + c.ph(f.Values[0]) + " AND " + c.ph(f.Values[1]) + " OR " + col + " IS NULL)"
	case "contains":
		return c.likeAny(col, f.Values, "%%%s%%", false)
	case "not contains":
		return c.likeAny(col, f.Values, "%%%s%%", true)
	case "starts with":
		return c.likeAny(col, f.Values, "%s%%", false)
	case "not starts with":
		return c.likeAny(col, f.Values, "%s%%", true)
	case "ends with":
		return c.likeAny(col, f.Values, "%%%s", false)
	case "not ends with":
		return c.likeAny(col, f.Values, "%%%s", true)
	case "is null":
		return col + " IS NULL"
	case "is not null":
		return col + " IS NOT NULL"
	default:
		// Unreachable via Compile. Emitted as invalid SQL on purpose: a
		// predicate this package cannot render must stop the query, not widen
		// it to every row.
		return fmt.Sprintf("/* semantic-go: unrendered operator %q */ 1 = 0", f.Op)
	}
}

// likeAny renders a LIKE over each value, OR-ed (or AND-ed NOT, for negation) —
// the shape Cube's contains/notContains operators carry.
func (c *compiler) likeAny(col string, values []any, pattern string, negate bool) string {
	var parts []string
	for _, v := range values {
		lit := c.ph(fmt.Sprintf(pattern, fmt.Sprint(v)))
		if negate {
			parts = append(parts, "("+col+" NOT LIKE "+lit+" OR "+col+" IS NULL)")
		} else {
			parts = append(parts, col+" LIKE "+lit)
		}
	}
	if len(parts) == 1 {
		return parts[0]
	}
	op := " OR "
	if negate {
		op = " AND "
	}
	return "(" + strings.Join(parts, op) + ")"
}

func (c *compiler) phList(values []any) string {
	var phs []string
	for _, v := range values {
		phs = append(phs, c.ph(v))
	}
	return strings.Join(phs, ", ")
}

// buildHaving compiles the post-aggregation filter tree against the outer
// query's metric expressions.
func (c *compiler) buildHaving() error {
	var rerr error
	render := func(f Filter) string { return "" }
	render = func(f Filter) string {
		if f.IsGroup() {
			return c.groupSQL(f, render)
		}
		expr, err := c.exprForMode(f.Metric, map[string]bool{}, false)
		if err != nil {
			if rerr == nil {
				rerr = err
			}
			return ""
		}
		return c.predicate(expr, f)
	}
	for _, f := range c.metricFilters {
		s := render(f)
		if rerr != nil {
			return rerr
		}
		if s != "" {
			c.havingPreds = append(c.havingPreds, s)
		}
	}
	return nil
}

func aggExpr(agg, expr string) (string, error) {
	switch strings.ToLower(agg) {
	case "sum":
		return "SUM(" + expr + ")", nil
	case "count":
		return "COUNT(" + expr + ")", nil
	case "count_distinct":
		return "COUNT(DISTINCT " + expr + ")", nil
	case "avg":
		return "AVG(" + expr + ")", nil
	case "min":
		return "MIN(" + expr + ")", nil
	case "max":
		return "MAX(" + expr + ")", nil
	default:
		return "", fmt.Errorf("unsupported agg %q", agg)
	}
}

type joinStep struct {
	rightTable string // physical table of the joined (child) entity
	rightAlias string // entity-name alias for it
	leftAlias  string // entity-name alias of the parent it joins to
	leftCols   []string
	rightCols  []string
}

// on renders the join condition, AND-ing every column of a composite key.
func (c *compiler) on(j joinStep) string {
	var parts []string
	for i := range j.leftCols {
		parts = append(parts, fmt.Sprintf("%s.%s = %s.%s",
			c.d.QuoteIdent(j.leftAlias), c.d.QuoteIdent(j.leftCols[i]),
			c.d.QuoteIdent(j.rightAlias), c.d.QuoteIdent(j.rightCols[i])))
	}
	return strings.Join(parts, " AND ")
}

// planJoins finds a safe (many-to-one only) join path from base to every needed
// entity, returning the joins in dependency order. A needed entity with no
// declared upward path is refused (no invented joins).
func (c *compiler) planJoins(base string, need map[string]bool) ([]joinStep, error) {
	adj := c.m.adjacency()

	// BFS from base, recording parent + edge used to reach each node.
	type crumb struct {
		parent string
		e      edge
	}
	came := map[string]crumb{}
	order := []string{}
	queue := []string{base}
	seen := map[string]bool{base: true}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, e := range adj[cur] {
			if seen[e.to] {
				continue
			}
			seen[e.to] = true
			came[e.to] = crumb{parent: cur, e: e}
			order = append(order, e.to)
			queue = append(queue, e.to)
		}
	}

	// Verify every needed entity is reachable.
	for ent := range need {
		if ent != base && !seen[ent] {
			return nil, fmt.Errorf("no declared join path from %q to %q (refused — add the relationship edge)%s",
				base, ent, c.m.explainUnreachable(base, ent))
		}
	}

	// Emit joins for needed entities + their intermediates, in BFS order.
	required := map[string]bool{}
	for ent := range need {
		for ent != base {
			required[ent] = true
			ent = came[ent].parent
		}
	}
	var steps []joinStep
	for _, ent := range order {
		if !required[ent] {
			continue
		}
		cb := came[ent]
		steps = append(steps, joinStep{
			rightTable: c.m.Entity(ent).Table,
			rightAlias: ent,
			leftAlias:  cb.parent,
			leftCols:   cb.e.leftCols,
			rightCols:  cb.e.rightCols,
		})
	}
	return steps, nil
}

func (c *compiler) assemble(ctes []string, outCols []string) string {
	var b strings.Builder
	b.WriteString("WITH " + strings.Join(ctes, ",\n") + "\n")

	dims := c.dims
	single := c.single
	first := c.cte(c.baseOrder[0])

	// dimension output
	var sel []string
	for _, d := range dims {
		sel = append(sel, c.dimRef(d)+" AS "+c.d.QuoteIdent(d.name))
	}
	sel = append(sel, outCols...)
	b.WriteString("SELECT " + strings.Join(sel, ", ") + "\n")

	switch {
	case single:
		b.WriteString("FROM " + first)

	case len(dims) == 0:
		// grand total: each CTE has exactly one row → CROSS JOIN is safe.
		b.WriteString("FROM " + first)
		for _, bm := range c.baseOrder[1:] {
			b.WriteString(" CROSS JOIN " + c.cte(bm))
		}

	default:
		// Build a dimension spine (UNION of each PROJECTED CTE's dim tuple),
		// then LEFT JOIN every metric CTE onto it. Avoids FULL JOIN (Postgres
		// rejects null-safe FULL JOIN conditions) while still keeping rows
		// present in any projected measure.
		//
		// The spine is projected-only on purpose: a measure named only in a
		// filter must be able to REMOVE groups, never to add them. Were its
		// dimension tuples UNION-ed in, "hours by region where cost > 50000"
		// would invent regions that have cost rows but no shift rows and show
		// them with zero hours — rows nobody asked for, each one a plausible
		// wrong answer.
		var dimCols []string
		for _, d := range dims {
			dimCols = append(dimCols, c.d.QuoteIdent(d.name))
		}
		var unions []string
		for _, bm := range c.baseOrder[:c.nProjected] {
			unions = append(unions, "SELECT "+strings.Join(dimCols, ", ")+" FROM "+c.cte(bm))
		}
		b.WriteString("FROM (" + strings.Join(unions, " UNION ") + ") " + c.keysAlias())
		for _, bm := range c.baseOrder {
			alias := c.cte(bm)
			var on []string
			for _, d := range dims {
				l := c.keysAlias() + "." + c.d.QuoteIdent(d.name)
				r := alias + "." + c.d.QuoteIdent(d.name)
				on = append(on, c.d.DistinctFrom(l, r))
			}
			b.WriteString(" LEFT JOIN " + alias + " ON " + strings.Join(on, " AND "))
		}
	}

	// Post-aggregation predicates. Each measure is already aggregated inside its
	// own CTE, so what is semantically a HAVING is physically a WHERE out here —
	// and it can name a measure that never appears in the SELECT list, which a
	// real HAVING on a flat GROUP BY could not do without re-aggregating.
	if len(c.havingPreds) > 0 {
		b.WriteString("\nWHERE " + strings.Join(c.havingPreds, " AND "))
	}

	hasOrder := c.q.OrderBy != ""
	if hasOrder {
		dir := ""
		if c.q.Descending {
			dir = " DESC"
		}
		b.WriteString("\nORDER BY " + c.d.QuoteIdent(c.q.OrderBy) + dir)
	}
	// Row limiting is the dialect's to spell: T-SQL has no LIMIT at all, and
	// the engines that have one disagree about whether OFFSET may appear
	// without it.
	if c.q.Limit > 0 {
		b.WriteString("\n" + c.d.LimitOffset(c.q.Limit, c.q.Offset, hasOrder))
	}
	return b.String()
}

// provenance snapshots what the compiler resolved, for the answer's paper trail.
func (c *compiler) provenance() Provenance {
	p := Provenance{Dialect: c.d.Name(), TimeGrain: c.q.TimeGrain, Joins: c.joinsUsed, Entities: c.entityUsed}
	for _, d := range c.dims {
		p.Dimensions = append(p.Dimensions, d.name)
	}
	for _, name := range c.q.Metrics {
		p.Metrics = append(p.Metrics, c.metricUse(name))
	}
	for _, name := range c.filterMetrics {
		p.FilterMetrics = append(p.FilterMetrics, c.metricUse(name))
	}
	return p
}

func (c *compiler) metricUse(name string) MetricUse {
	mt := c.m.Metric(name)
	if mt == nil {
		return MetricUse{Name: name}
	}
	u := MetricUse{
		Name:        mt.Name,
		Description: mt.Description,
		Entity:      mt.Entity,
		Agg:         mt.Agg,
		Expr:        mt.Expr,
		Formula:     mt.Formula,
		Window:      mt.Window,
		Additivity:  c.m.Additivity(mt.Name),
	}
	switch {
	case mt.IsWindow():
		u.Kind = "window"
	case mt.IsDerived():
		u.Kind = "derived"
	default:
		u.Kind = "base"
	}
	if u.Kind != "base" {
		sub := &compiler{m: c.m, baseSeen: map[string]bool{}}
		if err := sub.collectBases(name, map[string]bool{}); err == nil {
			u.BaseMetrics = sub.baseOrder
		}
	}
	return u
}

// firstDuplicate returns the first name that appears twice, or "".
func firstDuplicate(names []string) string {
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			return n
		}
		seen[n] = true
	}
	return ""
}

// containsName reports whether ss holds s.
func containsName(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
