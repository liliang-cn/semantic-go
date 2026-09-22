package semantic

import (
	"strings"
	"testing"
)

func shiftsModel(t *testing.T) *Model {
	t.Helper()
	m, err := LoadFile("testdata/shifts.yaml")
	if err != nil {
		t.Fatalf("load shifts model: %v", err)
	}
	return m
}

// The scope-boundary case from the spec: a measure projected from one fact
// table, filtered by a measure from ANOTHER fact table at a different grain,
// joined through a conformed dimension, and never itself projected. It exercises
// aggregate-then-join, multi-grain and HAVING-on-unprojected at once.
func TestFilterOnlyMeasureAcrossFactTables(t *testing.T) {
	m := shiftsModel(t)
	c, err := Compile(m, Query{
		Metrics: []string{"total_shift_hours"},
		GroupBy: []string{"guard_region"},
		Where: []Filter{
			{Dimension: "shift_type", Op: "=", Values: []any{"night"}},
			{Dimension: "shift_date", Op: "between", Values: []any{"2026-08-01", "2026-08-31"}},
			{Metric: "total_shift_cost", Op: ">", Values: []any{50000}},
		},
	}, DuckDB{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// Both measures aggregate in their own CTE, each at its own native grain.
	for _, want := range []string{`"m_total_shift_hours" AS (`, `"m_total_shift_cost" AS (`} {
		if !strings.Contains(c.SQL, want) {
			t.Errorf("expected a CTE %s in:\n%s", want, c.SQL)
		}
	}
	// The filtering measure must not be projected. Check the outer SELECT list
	// specifically — the name legitimately appears inside its own CTE.
	outer := outerSelect(c.SQL)
	if strings.Contains(outer, "total_shift_cost") {
		t.Errorf("filtering measure leaked into the projection:\n%s", outer)
	}
	// It is applied after aggregation, and NOT coalesced: a region with no cost
	// rows must drop out rather than compare as zero.
	if !strings.Contains(c.SQL, `WHERE "m_total_shift_cost"."total_shift_cost" > `) {
		t.Errorf("expected an un-coalesced post-aggregation predicate in:\n%s", c.SQL)
	}
	// The spine is built from the projected measure only, so the cost table
	// cannot introduce regions that have no shifts.
	spine := c.SQL[strings.Index(c.SQL, "FROM ("):]
	if strings.Contains(spine[:strings.Index(spine, `") "keys"`)], "m_total_shift_cost") {
		t.Errorf("filter-only measure was UNION-ed into the dimension spine:\n%s", c.SQL)
	}
	// The composite key is joined on both of its columns.
	if !strings.Contains(c.SQL, `"shift_pay"."guard_id" = "shift"."guard_id" AND "shift_pay"."shift_id" = "shift"."shift_id"`) {
		t.Errorf("composite join key was not joined in full:\n%s", c.SQL)
	}
	if got := len(c.Provenance.FilterMetrics); got != 1 {
		t.Errorf("provenance: want 1 filter metric, got %d", got)
	}
}

// A measure filter with nothing to group by is a HAVING on a single grand
// total: ambiguous rather than unsafe, and rejected rather than left to emerge.
func TestDegenerateHavingRefused(t *testing.T) {
	m := shiftsModel(t)
	_, err := Compile(m, Query{
		Metrics: []string{"total_shift_hours"},
		Where:   []Filter{{Metric: "total_shift_cost", Op: ">", Values: []any{50000}}},
	}, DuckDB{})
	if err == nil {
		t.Fatal("expected a refusal for a measure filter with no group_by")
	}
	if !strings.Contains(err.Error(), "grand total") {
		t.Errorf("refusal should explain the ambiguity, got: %v", err)
	}
}

// Pre- and post-aggregation predicates are different SQL clauses; no single
// clause expresses their disjunction, so a group holding both is refused.
func TestMixedFilterGroupRefused(t *testing.T) {
	m := shiftsModel(t)
	_, err := Compile(m, Query{
		Metrics: []string{"total_shift_hours"},
		GroupBy: []string{"guard_region"},
		Where: []Filter{{Or: []Filter{
			{Dimension: "shift_type", Op: "=", Values: []any{"night"}},
			{Metric: "total_shift_cost", Op: ">", Values: []any{50000}},
		}}},
	}, DuckDB{})
	if err == nil {
		t.Fatal("expected a refusal for a filter group mixing a dimension and a measure")
	}
	if !strings.Contains(err.Error(), "and/or group") {
		t.Errorf("unhelpful refusal: %v", err)
	}
}

// A bridge table makes shift×site many-to-many. The path exists; taking it
// would multiply every shift by the sites it covered, so it is refused — and
// the refusal names the path, so the modeller knows which edge to look at.
func TestBridgePathRefused(t *testing.T) {
	m := shiftsModel(t)
	_, err := Compile(m, Query{
		Metrics: []string{"total_shift_hours"},
		GroupBy: []string{"client_site_category"},
	}, DuckDB{})
	if err == nil {
		t.Fatal("expected a refusal for a dimension only reachable across a bridge")
	}
	for _, want := range []string{"no declared join path", "site_assignment"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should mention %q, got: %v", want, err)
		}
	}
}

// Two fact tables with no conformed dimension between them is a chasm trap.
// There is no path at all, so the refusal has no route to name — and says so
// without inventing one.
func TestFactToFactWithoutConformedDimensionRefused(t *testing.T) {
	m := shiftsModel(t)
	_, err := Compile(m, Query{
		Metrics: []string{"total_shift_hours", "total_invoiced"},
		GroupBy: []string{"guard_region"},
		Roles:   []string{"finance"},
	}, DuckDB{})
	if err == nil {
		t.Fatal("expected a refusal joining two facts with no conformed dimension")
	}
	if !strings.Contains(err.Error(), `from "invoice" to "guard"`) {
		t.Errorf("refusal should name the missing edge, got: %v", err)
	}
}

// Measures at differing native grains, both projected, conformed onto one
// dimension: supported, and each aggregates before the join.
func TestMultiGrainProjection(t *testing.T) {
	m := shiftsModel(t)
	c, err := Compile(m, Query{
		Metrics: []string{"total_shift_hours", "total_shift_cost", "cost_per_hour"},
		GroupBy: []string{"guard_region"},
	}, DuckDB{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !strings.Contains(c.SQL, "UNION") {
		t.Errorf("expected a UNION-ed dimension spine for two projected measures:\n%s", c.SQL)
	}
	if n := strings.Count(c.SQL, " AS (\n  SELECT "); n != 2 {
		t.Errorf("want 2 aggregation CTEs, got %d:\n%s", n, c.SQL)
	}
}

// A window function cannot appear in a WHERE clause. Refuse, and say what to
// filter instead, rather than emitting SQL the engine will reject.
func TestWindowMeasureFilterRefused(t *testing.T) {
	m := shiftsModel(t)
	_, err := Compile(m, Query{
		Metrics:   []string{"total_shift_hours"},
		GroupBy:   []string{"shift_date"},
		TimeGrain: "month",
		Where:     []Filter{{Metric: "hours_cumulative", Op: ">", Values: []any{10}}},
	}, DuckDB{})
	if err == nil {
		t.Fatal("expected a refusal filtering on a window metric")
	}
	if !strings.Contains(err.Error(), "window") {
		t.Errorf("unhelpful refusal: %v", err)
	}
}

// Roles gate transitively: a public metric whose formula references a
// restricted one must not launder it into view.
func TestRoleGate(t *testing.T) {
	m := shiftsModel(t)
	q := Query{Metrics: []string{"total_invoiced"}, GroupBy: []string{"client_name"}}
	if _, err := Compile(m, q, DuckDB{}); err == nil {
		t.Fatal("expected a refusal without the finance role")
	}
	q.Roles = []string{"finance"}
	if _, err := Compile(m, q, DuckDB{}); err != nil {
		t.Fatalf("finance should be admitted: %v", err)
	}
	if got := m.VisibleMetricNames(nil); containsName(got, "total_invoiced") {
		t.Errorf("restricted metric listed to a role-less caller: %v", got)
	}
}

// A dataset whose grain test failed is withheld — from aggregation and from
// being joined through, since a duplicated row on the one-side of a join fans
// out just as surely as an undeclared one-to-many edge.
func TestFailedGrainWithholdsDataset(t *testing.T) {
	m := shiftsModel(t)
	m.Entity("guard").GrainStatus = GrainFail
	_, err := Compile(m, Query{Metrics: []string{"total_shift_hours"}, GroupBy: []string{"guard_region"}}, DuckDB{})
	if err == nil {
		t.Fatal("expected a refusal joining through a dataset whose grain test failed")
	}
	if !strings.Contains(err.Error(), "grain test failed") {
		t.Errorf("unhelpful refusal: %v", err)
	}
	gi, err := m.DescribeGrain("guard")
	if err != nil {
		t.Fatal(err)
	}
	if gi.Usable {
		t.Error("describe_grain should report the dataset as unusable")
	}
}

// list_dimensions, called with every metric a question needs, returns the
// intersection — plus the dimensions a reader would expect to be groupable and
// the declared edge that excludes them.
func TestDimensionReport(t *testing.T) {
	m := shiftsModel(t)
	avail, excl, err := m.DimensionReport([]string{"total_shift_hours", "total_shift_cost"}, nil)
	if err != nil {
		t.Fatalf("DimensionReport: %v", err)
	}
	names := map[string]DimAvailability{}
	for _, a := range avail {
		names[a.Name] = a
	}
	for _, want := range []string{"guard_region", "shift_type", "shift_date"} {
		if _, ok := names[want]; !ok {
			t.Errorf("%s should be groupable by both measures; got %v", want, avail)
		}
	}
	// pay_component is native to shift_pay and unreachable from shift, so the
	// intersection must drop it.
	if _, ok := names["pay_component"]; ok {
		t.Error("pay_component is not reachable from the shift fact and must not be offered")
	}
	if names["shift_date"].IsTime != true {
		t.Error("shift_date should be flagged as a time dimension")
	}
	if names["guard_region"].Join == "native" || names["guard_region"].Join == "" {
		t.Errorf("guard_region should report its join path, got %q", names["guard_region"].Join)
	}
	var found bool
	for _, e := range excl {
		if e.Name == "client_site_category" {
			found = true
			if !strings.Contains(e.Reason, "site_assignment") {
				t.Errorf("exclusion should name the bridge path, got %q", e.Reason)
			}
		}
	}
	if !found {
		t.Errorf("client_site_category should be listed as excluded, got %v", excl)
	}
	// Unrelated dimensions are not enumerated: the list names guardrails a
	// reader would mistake for gaps, not every column in the model.
	for _, e := range excl {
		if e.Name == "client_name" {
			t.Errorf("unrelated dimension %q should not be in the exclusion list", e.Name)
		}
		if len(e.Metrics) == 0 {
			t.Errorf("exclusion %q names no metric it is unavailable for", e.Name)
		}
	}
}

// outerSelect returns the projection list of the assembled query — the SELECT
// that begins a line, not one nested inside a CTE or the dimension spine.
func outerSelect(sql string) string {
	for _, line := range strings.Split(sql, "\n") {
		if strings.HasPrefix(line, "SELECT ") {
			return line
		}
	}
	return ""
}

// An expression the modeller qualified is left exactly as written, and quoted
// regions are never rewritten.
func TestQualifyExprLeavesAuthoredSQLAlone(t *testing.T) {
	d := DuckDB{}
	cases := []struct{ in, want string }{
		{"hours_worked", `"shift"."hours_worked"`},
		{"quantity * unit_price", `"shift"."quantity" * "shift"."unit_price"`},
		{"shifts.hours_worked", "shifts.hours_worked"},
		{"COALESCE(amount, 0)", `COALESCE("shift"."amount", 0)`},
		{"CASE WHEN status = 'paid' THEN amount ELSE 0 END",
			`CASE WHEN "shift"."status" = 'paid' THEN "shift"."amount" ELSE 0 END`},
		{"CAST(x AS DECIMAL(10,2))", `CAST("shift"."x" AS DECIMAL(10,2))`},
		{"*", "*"},
	}
	for _, tc := range cases {
		if got := qualifyExpr(tc.in, "shift", d); got != tc.want {
			t.Errorf("qualifyExpr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A level — inventory, a balance, a headcount — may be summed across anything
// except time. Nothing about the wrong answer looks wrong: a month of daily
// snapshots added together is roughly thirty times the stock, a large plausible
// number among large plausible numbers.
func semiAdditiveModel(t *testing.T) *Model {
	t.Helper()
	m := &Model{
		Entities: []Entity{
			{Name: "snap", Table: "inventory_snapshots", PrimaryKey: StringList{"sku", "d"}, GrainStatus: GrainPass},
			{Name: "item", Table: "items", PrimaryKey: StringList{"sku"}, GrainStatus: GrainPass},
		},
		Joins: []Join{{From: "snap", To: "item", FromKey: StringList{"sku"}, ToKey: StringList{"sku"}, Cardinality: "many_to_one"}},
		Dimensions: []Dimension{
			{Name: "snap_date", Entity: "snap", Column: "d", Type: "time"},
			{Name: "received_on", Entity: "snap", Column: "received_at", Type: "time"}, // NOT part of the grain
			{Name: "category", Entity: "item", Column: "category", Type: "categorical"},
		},
		Metrics: []Metric{
			{Name: "on_hand", Description: "Units on hand at a point in time.", Synonyms: []string{"stock"},
				Entity: "snap", Agg: "sum", Expr: "qty", Additivity: SemiAdditive},
		},
	}
	if err := m.Index(); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSemiAdditiveGate(t *testing.T) {
	m := semiAdditiveModel(t)
	cases := []struct {
		name string
		q    Query
		want string // substring of the refusal; "" means it must compile
	}{
		{"grand total sums every period together",
			Query{Metrics: []string{"on_hand"}}, "across all of time"},
		{"a coarser grain buckets several snapshots",
			Query{Metrics: []string{"on_hand"}, GroupBy: []string{"snap_date"}, TimeGrain: "month"}, "buckets several snapshots"},
		{"a time dimension outside the declared grain",
			Query{Metrics: []string{"on_hand"}, GroupBy: []string{"received_on"}}, "declared grain"},
		{"summed across categories only, still across all time",
			Query{Metrics: []string{"on_hand"}, GroupBy: []string{"category"}}, "across all of time"},
		{"a range is not a point in time",
			Query{Metrics: []string{"on_hand"}, GroupBy: []string{"category"},
				Where: []Filter{{Dimension: "snap_date", Op: "between", Values: []any{"2026-08-01", "2026-08-31"}}}}, "across all of time"},

		// The safe shapes: one group is one snapshot.
		{"grouped at the snapshot's own grain",
			Query{Metrics: []string{"on_hand"}, GroupBy: []string{"snap_date", "category"}}, ""},
		{"pinned to one date, then summed across products",
			Query{Metrics: []string{"on_hand"}, GroupBy: []string{"category"},
				Where: []Filter{{Dimension: "snap_date", Op: "=", Values: []any{"2026-08-31"}}}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(m, tc.q, DuckDB{})
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("should compile: %v", err)
			case tc.want != "" && err == nil:
				t.Fatal("expected a refusal")
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Errorf("refusal should mention %q, got: %v", tc.want, err)
			}
		})
	}

	// The gate must not fire on measures that are not levels.
	plain := semiAdditiveModel(t)
	plain.Metrics[0].Additivity = Additive
	if err := plain.Index(); err != nil {
		t.Fatal(err)
	}
	if _, err := Compile(plain, Query{Metrics: []string{"on_hand"}}, DuckDB{}); err != nil {
		t.Errorf("an additive measure must still total: %v", err)
	}
	// And a correctly modelled level must pass the build-time gate, or the gate
	// cries wolf and gets turned off.
	if errs := LintErrors(m); len(errs) > 0 {
		t.Errorf("a correctly modelled semi-additive metric should pass Lint, got %v", errs)
	}
}

// A bare word in a formula that names no metric is a typo. Left alone it
// reaches the warehouse as a column reference.
func TestFormulaTypoRefused(t *testing.T) {
	m := shiftsModel(t)
	m.Metrics = append(m.Metrics, Metric{
		Name: "bad", Description: "d", Synonyms: []string{"b"},
		Formula: "total_shift_cost - shrinkaage",
	})
	if err := m.Index(); err != nil {
		t.Fatal(err)
	}
	_, err := Compile(m, Query{Metrics: []string{"bad"}, GroupBy: []string{"guard_region"}}, DuckDB{})
	if err == nil || !strings.Contains(err.Error(), "shrinkaage") {
		t.Fatalf("expected the typo to be named, got: %v", err)
	}
	var found bool
	for _, i := range LintErrors(m) {
		if i.Target == "bad" && strings.Contains(i.Message, "name no metric") {
			found = true
			if !strings.Contains(i.Message, "did you mean") {
				t.Errorf("the gate should suggest a fix, got %q", i.Message)
			}
			if strings.Contains(i.Message, `"bad"`) {
				t.Errorf("suggesting the metric itself would be a cycle, not a fix: %q", i.Message)
			}
		}
	}
	if !found {
		t.Error("Lint should catch the typo at build time, not leave it to the warehouse")
	}
	// Function calls and literals must still pass through untouched.
	if bad := m.unknownFormulaRefs("total_shift_cost / nullif(total_shift_hours, 0) + 1.5"); len(bad) > 0 {
		t.Errorf("a function call is not an unknown metric: %v", bad)
	}
}

// Masking is a projection, not a filter: the rows stay, the totals stay
// truthful, and the identifying value does not leave the warehouse.
func TestDimensionMasking(t *testing.T) {
	m := shiftsModel(t)

	masked, err := Compile(m, Query{Metrics: []string{"total_shift_hours"}, GroupBy: []string{"guard_name"}}, DuckDB{})
	if err != nil {
		t.Fatalf("a masked dimension must still be groupable: %v", err)
	}
	// The mask must of course READ the column in order to mask it. What must
	// not happen is the raw value being projected or grouped by: masking only
	// the output would leak the distinctness of the hidden values — one row per
	// real guard, every one labelled the same.
	if strings.Contains(masked.SQL, `"guard"."name" AS "guard_name"`) {
		t.Errorf("the raw value was projected:\n%s", masked.SQL)
	}
	if strings.Contains(masked.SQL, `GROUP BY "guard"."name"`) {
		t.Errorf("grouping by the raw column leaks how many distinct values there are:\n%s", masked.SQL)
	}
	if !strings.Contains(masked.SQL, `GROUP BY substr("guard"."name", 1, 1)`) {
		t.Errorf("the mask should be grouped by, and its bare column qualified:\n%s", masked.SQL)
	}

	raw, err := Compile(m, Query{Metrics: []string{"total_shift_hours"}, GroupBy: []string{"guard_name"}, Roles: []string{"rostering"}}, DuckDB{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw.SQL, `"guard"."name"`) {
		t.Errorf("a caller holding the role should see the raw column:\n%s", raw.SQL)
	}

	// Filtering a masked dimension would turn the predicate into an oracle:
	// ask for 'a', then 'b', and the row counts read the value back out.
	_, err = Compile(m, Query{
		Metrics: []string{"total_shift_hours"}, GroupBy: []string{"guard_region"},
		Where: []Filter{{Dimension: "guard_name", Op: "starts with", Values: []any{"A"}}},
	}, DuckDB{})
	if err == nil {
		t.Fatal("expected a refusal filtering on a masked dimension")
	}
	if !strings.Contains(err.Error(), "one comparison at a time") {
		t.Errorf("the refusal should say why, got: %v", err)
	}
	// The same filter is fine for someone who may see the values.
	if _, err := Compile(m, Query{
		Metrics: []string{"total_shift_hours"}, GroupBy: []string{"guard_region"},
		Roles: []string{"rostering"},
		Where: []Filter{{Dimension: "guard_name", Op: "starts with", Values: []any{"A"}}},
	}, DuckDB{}); err != nil {
		t.Errorf("rostering may filter on the name it can see: %v", err)
	}
}

// A mask with no roles is masked for everybody, and roles with no mask leave
// nothing to show. Both are modelling mistakes, caught at load.
func TestMaskAndRolesMustAgree(t *testing.T) {
	base := func(d Dimension) *Model {
		return &Model{
			Entities:   []Entity{{Name: "e", Table: "t", PrimaryKey: StringList{"id"}}},
			Dimensions: []Dimension{d},
			Metrics:    []Metric{{Name: "m", Description: "d", Entity: "e", Agg: "count", Expr: "id"}},
		}
	}
	if err := base(Dimension{Name: "x", Entity: "e", Column: "x", Type: "categorical", Mask: "'***'"}).Index(); err == nil {
		t.Error("a mask with no roles hides the value from the people it is meant for")
	}
	if err := base(Dimension{Name: "x", Entity: "e", Column: "x", Type: "categorical", Roles: []string{"a"}}).Index(); err == nil {
		t.Error("roles with no mask leave nothing to show a caller who lacks them")
	}
}
