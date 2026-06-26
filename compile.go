package semantic

import (
	"fmt"
	"regexp"
	"strings"
)

// Compiled is the output: SQL plus ordered bind arguments.
type Compiled struct {
	SQL  string
	Args []any
}

// Compile turns a semantic Query into fanout/chasm-safe SQL for the dialect.
//
// Algorithm: each base metric is aggregated to the requested dimension grain in
// its OWN CTE (joining only upward via many-to-one edges, so the leaf grain is
// never multiplied); the CTEs are then null-safe outer-joined on the shared
// dimensions and combined (derived formulas computed in the outer SELECT). This
// is what makes fan-out and chasm joins impossible by construction.
func Compile(m *Model, q Query, d Dialect) (Compiled, error) {
	if len(q.Metrics) == 0 {
		return Compiled{}, fmt.Errorf("query has no metrics")
	}
	c := &compiler{m: m, q: q, d: d, baseSeen: map[string]bool{}}

	// 1) Resolve group-by dimensions (needed by window metrics).
	if err := c.resolveDims(q.GroupBy); err != nil {
		return Compiled{}, err
	}

	// 2) Pass 1 — classify: collect the base metrics every requested metric
	//    depends on (through derived formulas and window `of`), without emitting SQL.
	for _, name := range q.Metrics {
		if err := c.collectBases(name, map[string]bool{}); err != nil {
			return Compiled{}, err
		}
	}
	c.single = len(c.baseOrder) == 1

	// 3) Pass 2 — build the outer-SELECT expression for each requested metric.
	outCols := make([]string, 0, len(q.Metrics))
	for _, name := range q.Metrics {
		expr, err := c.exprFor(name, map[string]bool{})
		if err != nil {
			return Compiled{}, err
		}
		outCols = append(outCols, expr+" AS "+d.QuoteIdent(name))
	}

	// 4) Build one CTE per base metric (aggregate-to-grain-first).
	var ctes []string
	for _, bm := range c.baseOrder {
		cte, err := c.buildCTE(bm, c.dims)
		if err != nil {
			return Compiled{}, err
		}
		ctes = append(ctes, cte)
	}

	// 5) Assemble the outer query joining the CTEs on the dimensions.
	sql := c.assemble(ctes, outCols)
	return Compiled{SQL: sql, Args: c.args}, nil
}

type compiler struct {
	m         *Model
	q         Query
	d         Dialect
	args      []any
	baseOrder []string // base metrics in first-seen order
	baseSeen  map[string]bool
	dims      []resolvedDim
	single    bool
}

// dimRef is how a dimension column is referenced in the outer SELECT (and in
// window OVER clauses): from the single base CTE, or the UNION-ed `keys` spine.
func (c *compiler) dimRef(d resolvedDim) string {
	if c.single {
		return "m_" + c.baseOrder[0] + "." + c.d.QuoteIdent(d.name)
	}
	return "keys." + c.d.QuoteIdent(d.name)
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

var identRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// collectBases records every base (simple-aggregation) metric a requested metric
// depends on, recursing through derived formulas and window `of`.
func (c *compiler) collectBases(name string, visiting map[string]bool) error {
	mt := c.m.Metric(name)
	if mt == nil {
		return fmt.Errorf("unknown metric %q", name)
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

// exprFor returns the outer-SELECT SQL for a requested metric.
func (c *compiler) exprFor(name string, visiting map[string]bool) (string, error) {
	mt := c.m.Metric(name)
	if mt == nil {
		return "", fmt.Errorf("unknown metric %q", name)
	}
	switch {
	case mt.IsWindow():
		return c.windowExpr(mt)
	case mt.IsDerived():
		if visiting[name] {
			return "", fmt.Errorf("derived metric %q has a cyclic formula", name)
		}
		visiting[name] = true
		defer delete(visiting, name)
		var rerr error
		out := identRe.ReplaceAllStringFunc(mt.Formula, func(tok string) string {
			if c.m.Metric(tok) == nil {
				return tok // SQL function/literal
			}
			sub, err := c.exprFor(tok, visiting)
			if err != nil {
				rerr = err
				return tok
			}
			return sub
		})
		if rerr != nil {
			return "", rerr
		}
		return "(" + out + ")", nil
	default:
		return fmt.Sprintf("COALESCE(m_%s.%s, 0)", name, c.d.QuoteIdent(name)), nil
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
	for _, n := range names {
		dim := c.m.Dimension(n)
		if dim == nil {
			return fmt.Errorf("unknown dimension %q", n)
		}
		raw := c.qualify(dim.Entity, dim.Column)
		expr := raw
		if dim.Type == "time" && c.q.TimeGrain != "" {
			expr = c.d.DateTrunc(c.q.TimeGrain, raw)
		}
		out = append(out, resolvedDim{name: n, entity: dim.Entity, typ: dim.Type, sqlRaw: raw, sql: expr})
	}
	c.dims = out
	return nil
}

func (c *compiler) qualify(entity, col string) string {
	t := c.m.Entity(entity).Table
	return c.d.QuoteIdent(t) + "." + c.d.QuoteIdent(col)
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
	for _, f := range c.q.Where {
		fd := c.m.Dimension(f.Dimension)
		if fd == nil {
			return "", fmt.Errorf("unknown dimension %q in filter", f.Dimension)
		}
		need[fd.Entity] = true
	}
	joins, err := c.planJoins(base, need)
	if err != nil {
		return "", err
	}

	agg, err := aggExpr(mt.Agg, mt.Expr)
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
	fmt.Fprintf(&b, "m_%s AS (\n  SELECT %s\n  FROM %s", metricName, strings.Join(sel, ", "),
		c.d.QuoteIdent(c.m.Entity(base).Table))
	for _, j := range joins {
		fmt.Fprintf(&b, "\n  JOIN %s ON %s.%s = %s.%s",
			c.d.QuoteIdent(j.rightTable),
			c.d.QuoteIdent(j.leftTable), c.d.QuoteIdent(j.leftCol),
			c.d.QuoteIdent(j.rightTable), c.d.QuoteIdent(j.rightCol))
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

func (c *compiler) buildWhere() string {
	var preds []string
	for _, f := range c.q.Where {
		fd := c.m.Dimension(f.Dimension)
		col := c.qualify(fd.Entity, fd.Column)
		switch strings.ToLower(f.Op) {
		case "in":
			var phs []string
			for _, v := range f.Values {
				phs = append(phs, c.ph(v))
			}
			preds = append(preds, col+" IN ("+strings.Join(phs, ", ")+")")
		case "=", "!=", ">", ">=", "<", "<=":
			v := any(nil)
			if len(f.Values) > 0 {
				v = f.Values[0]
			}
			preds = append(preds, col+" "+f.Op+" "+c.ph(v))
		}
	}
	return strings.Join(preds, " AND ")
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
	rightTable string
	leftTable  string
	leftCol    string
	rightCol   string
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
			return nil, fmt.Errorf("no declared join path from %q to %q (refused — add the relationship edge)", base, ent)
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
			leftTable:  c.m.Entity(cb.parent).Table,
			leftCol:    cb.e.leftCol,
			rightCol:   cb.e.rightCol,
		})
	}
	return steps, nil
}

func (c *compiler) assemble(ctes []string, outCols []string) string {
	var b strings.Builder
	b.WriteString("WITH " + strings.Join(ctes, ",\n") + "\n")

	dims := c.dims
	single := c.single
	first := "m_" + c.baseOrder[0]

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
			b.WriteString(" CROSS JOIN m_" + bm)
		}

	default:
		// Build a dimension spine (UNION of each CTE's dim tuple), then LEFT JOIN
		// each metric CTE onto it. Avoids FULL JOIN (Postgres rejects null-safe
		// FULL JOIN conditions) while still keeping rows present in any CTE.
		var dimCols []string
		for _, d := range dims {
			dimCols = append(dimCols, c.d.QuoteIdent(d.name))
		}
		var unions []string
		for _, bm := range c.baseOrder {
			unions = append(unions, "SELECT "+strings.Join(dimCols, ", ")+" FROM m_"+bm)
		}
		b.WriteString("FROM (" + strings.Join(unions, " UNION ") + ") keys")
		for _, bm := range c.baseOrder {
			alias := "m_" + bm
			var on []string
			for _, d := range dims {
				l := "keys." + c.d.QuoteIdent(d.name)
				r := alias + "." + c.d.QuoteIdent(d.name)
				on = append(on, c.d.DistinctFrom(l, r))
			}
			b.WriteString(" LEFT JOIN " + alias + " ON " + strings.Join(on, " AND "))
		}
	}

	if c.q.OrderBy != "" {
		dir := ""
		if c.q.Descending {
			dir = " DESC"
		}
		b.WriteString("\nORDER BY " + c.d.QuoteIdent(c.q.OrderBy) + dir)
	}
	if c.q.Limit > 0 {
		fmt.Fprintf(&b, "\nLIMIT %d", c.q.Limit)
	}
	return b.String()
}
