package semantic

import (
	"fmt"
	"strings"
	"testing"
)

func robustModel(t *testing.T) *Model {
	t.Helper()
	m := &Model{
		Name: "p", Description: "d",
		Entities: []Entity{
			{Name: "oi", Table: "order_items", PrimaryKey: StringList{"id"}, GrainStatus: GrainPass},
			{Name: "o", Table: "orders", PrimaryKey: StringList{"order_id"}, GrainStatus: GrainPass},
		},
		Joins: []Join{{From: "oi", To: "o", FromKey: StringList{"order_id"}, ToKey: StringList{"order_id"}, Cardinality: "many_to_one"}},
		Dimensions: []Dimension{
			{Name: "status", Entity: "o", Column: "status", Type: "categorical"},
			{Name: "od", Entity: "o", Column: "order_date", Type: "time"},
		},
		Metrics: []Metric{
			{Name: "rev", Description: "d", Synonyms: []string{"r"}, Entity: "oi", Agg: "sum", Expr: "qty*price"},
		},
	}
	if err := m.Index(); err != nil {
		t.Fatal(err)
	}
	return m
}

// A malformed filter used to compile to NOTHING — no SQL and no error — so a
// query that asked to be filtered came back unfiltered, with a number that was
// larger than it should have been and entirely plausible. That is worse than a
// fan-out, which at least multiplies by something a reader might notice.
func TestMalformedFiltersAreRefusedNotDropped(t *testing.T) {
	m := robustModel(t)
	cases := []struct {
		name string
		f    Filter
		want string
	}{
		{"no operator", Filter{Dimension: "status"}, "has no operator"},
		{"unknown operator", Filter{Dimension: "status", Op: "~=", Values: []any{"x"}}, "unknown operator"},
		{"in with no values", Filter{Dimension: "status", Op: "in"}, "at least 1"},
		{"between with one bound", Filter{Dimension: "od", Op: "between", Values: []any{"2026-01-01"}}, "exactly 2"},
		{"between with three bounds", Filter{Dimension: "od", Op: "between", Values: []any{"a", "b", "c"}}, "exactly 2"},
		{"equality against null", Filter{Dimension: "status", Op: "=", Values: []any{nil}}, "never true"},
		{"is null given a value", Filter{Dimension: "status", Op: "is null", Values: []any{"x"}}, "needs none"},
		{"no member at all", Filter{Op: "=", Values: []any{"x"}}, "neither a dimension nor a metric"},
		{"both a dimension and a metric", Filter{Dimension: "status", Metric: "rev", Op: "=", Values: []any{"x"}}, "one or the other"},
		{"a member and a group", Filter{Dimension: "status", Op: "=", Values: []any{"x"},
			And: []Filter{{Dimension: "status", Op: "=", Values: []any{"y"}}}}, "and/or group"},
		{"unknown dimension", Filter{Dimension: "nope", Op: "=", Values: []any{"x"}}, "unknown dimension"},
		{"unknown metric", Filter{Metric: "revv", Op: ">", Values: []any{1}}, "did you mean"},
		{"malformed inside a group", Filter{Or: []Filter{
			{Dimension: "status", Op: "=", Values: []any{"x"}},
			{Dimension: "status"},
		}}, "has no operator"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(m, Query{Metrics: []string{"rev"}, GroupBy: []string{"status"}, Where: []Filter{tc.f}}, DuckDB{})
			if err == nil {
				t.Fatal("expected a refusal — a dropped filter is a silently wrong number")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want a refusal mentioning %q, got: %v", tc.want, err)
			}
		})
	}
}

// Every declared operator must render to SQL. A branch that falls through to ""
// is how the whole class of bug above happened.
func TestEveryOperatorRenders(t *testing.T) {
	m := robustModel(t)
	values := map[string][]any{
		"is null": nil, "is not null": nil,
		"between": {"a", "b"}, "not between": {"a", "b"},
	}
	for _, op := range FilterOperators() {
		t.Run(op, func(t *testing.T) {
			vals, ok := values[op]
			if !ok {
				vals = []any{"x"}
			}
			c, err := Compile(m, Query{
				Metrics: []string{"rev"}, GroupBy: []string{"status"},
				Where: []Filter{{Dimension: "status", Op: op, Values: vals}},
			}, DuckDB{})
			if err != nil {
				t.Fatalf("declared operator does not compile: %v", err)
			}
			if !strings.Contains(c.SQL, "WHERE ") {
				t.Errorf("operator %q produced no predicate:\n%s", op, c.SQL)
			}
			if strings.Contains(c.SQL, "unrendered operator") {
				t.Errorf("operator %q is declared but not rendered:\n%s", op, c.SQL)
			}
		})
	}
	// Case and padding are not a different operator.
	if _, err := Compile(m, Query{Metrics: []string{"rev"}, GroupBy: []string{"status"},
		Where: []Filter{{Dimension: "status", Op: " IN ", Values: []any{"x"}}}}, DuckDB{}); err != nil {
		t.Errorf("operators should be case- and space-insensitive: %v", err)
	}
}

// A declared name that resolves to nothing is worse than a missing one: it is
// in the file, and which of the two duplicates survived was an accident of
// order.
func TestDuplicateDeclaredNamesRefused(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Model)
		want string
	}{
		{"two entities", func(m *Model) {
			m.Entities = append(m.Entities, Entity{Name: "o", Table: "other", PrimaryKey: StringList{"x"}})
		}, "both named \"o\""},
		{"two dimensions", func(m *Model) {
			m.Dimensions = append(m.Dimensions, Dimension{Name: "status", Entity: "o", Column: "other", Type: "categorical"})
		}, "both named \"status\""},
		{"two metrics", func(m *Model) {
			m.Metrics = append(m.Metrics, Metric{Name: "rev", Description: "other", Entity: "oi", Agg: "count", Expr: "id"})
		}, "both named \"rev\""},
		{"a metric and a dimension", func(m *Model) {
			m.Dimensions = append(m.Dimensions, Dimension{Name: "rev", Entity: "o", Column: "flag", Type: "categorical"})
		}, "both a metric and a dimension"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := robustModel(t)
			tc.mut(m)
			err := m.Index()
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want %q, got: %v", tc.want, err)
			}
		})
	}
}

// Two columns under one name is ambiguous for whatever reads the result back.
func TestRepeatedRequestRefused(t *testing.T) {
	m := robustModel(t)
	if _, err := Compile(m, Query{Metrics: []string{"rev", "rev"}, GroupBy: []string{"status"}}, DuckDB{}); err == nil {
		t.Error("the same metric twice should be refused")
	}
	if _, err := Compile(m, Query{Metrics: []string{"rev"}, GroupBy: []string{"status", "status"}}, DuckDB{}); err == nil {
		t.Error("the same dimension twice should be refused")
	}
	// A synonym and its canonical name are the same request.
	q := Query{Metrics: []string{"r", "rev"}, GroupBy: []string{"status"}}
	if err := m.ResolveMetrics(&q); err != nil {
		t.Fatal(err)
	}
	if _, err := Compile(m, q, DuckDB{}); err == nil {
		t.Error("a synonym and its canonical name are one metric, asked for twice")
	}
}

// ORDER BY names an output column, so it has to be one — checked here rather
// than by the engine, about generated SQL, later.
func TestOrderByMustBeProjected(t *testing.T) {
	m := robustModel(t)
	_, err := Compile(m, Query{Metrics: []string{"rev"}, GroupBy: []string{"status"}, OrderBy: "od", Limit: 5}, DuckDB{})
	if err == nil {
		t.Fatal("expected a refusal: od is in the model but not in this result")
	}
	if !strings.Contains(err.Error(), "available:") {
		t.Errorf("the refusal should list what can be ordered by, got: %v", err)
	}
	for _, name := range []string{"rev", "status"} {
		if _, err := Compile(m, Query{Metrics: []string{"rev"}, GroupBy: []string{"status"}, OrderBy: name, Limit: 5}, DuckDB{}); err != nil {
			t.Errorf("order_by %q should be allowed: %v", name, err)
		}
	}
}

// A window over a window emits a window function inside another. It used to be
// caught only incidentally, through inferred additivity, so declaring
// `additivity: additive` walked straight past the guard.
func TestNestedWindowRefused(t *testing.T) {
	m := robustModel(t)
	m.Metrics = append(m.Metrics,
		Metric{Name: "roll", Description: "d", Of: "rev", Window: "rolling:3", Additivity: Additive},
		Metric{Name: "nested", Description: "d", Of: "roll", Window: "cumulative", Additivity: Additive},
	)
	err := m.Index()
	if err == nil {
		t.Fatal("expected the model to be refused at load")
	}
	if !strings.Contains(err.Error(), "window over window") {
		t.Errorf("the refusal should name the nesting, got: %v", err)
	}

	// A window over a base or derived metric is fine.
	ok := robustModel(t)
	ok.Metrics = append(ok.Metrics, Metric{Name: "roll", Description: "d", Of: "rev", Window: "rolling:3"})
	if err := ok.Index(); err != nil {
		t.Errorf("a window over a base metric is the normal case: %v", err)
	}
	// And a window pointing at nothing is named, not left to the compiler.
	bad := robustModel(t)
	bad.Metrics = append(bad.Metrics, Metric{Name: "w", Description: "d", Of: "ghost", Window: "cumulative"})
	if err := bad.Index(); err == nil || !strings.Contains(err.Error(), "not a metric") {
		t.Errorf("want a refusal naming the missing base, got: %v", err)
	}
}

// A Model is read-only after Index, and Index now memoizes the join graphs and
// every entity's reachable set. Those caches are shared, so nothing downstream
// may narrow one in place — DimensionsFor used to do exactly that, intersecting
// the map it was handed. One server serving concurrent requests would have
// watched the set of legal dimensions shrink as it ran.
func TestModelIsSafeForConcurrentReads(t *testing.T) {
	m := shiftsModel(t)
	metrics := []string{"total_shift_hours", "total_shift_cost"}

	// Two measures on different datasets: the intersection is narrower than
	// either one's reachable set, which is what makes an in-place narrowing
	// visible.
	wantDims, err := m.DimensionsFor("total_shift_hours")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 64)
	for i := 0; i < 64; i++ {
		go func() {
			if _, _, err := m.DimensionReport(metrics, nil); err != nil {
				done <- err
				return
			}
			if _, err := Compile(m, Query{Metrics: []string{"total_shift_hours"}, GroupBy: []string{"guard_region"}}, DuckDB{}); err != nil {
				done <- err
				return
			}
			if _, err := m.DimensionsFor("total_shift_cost"); err != nil {
				done <- err
				return
			}
			done <- nil
		}()
	}
	for i := 0; i < 64; i++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent read failed: %v", err)
		}
	}

	// And the answer is the same afterwards as before: nothing was consumed.
	gotDims, err := m.DimensionsFor("total_shift_hours")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(gotDims, ",") != strings.Join(wantDims, ",") {
		t.Errorf("the reachable set was narrowed in place: %v then %v", wantDims, gotDims)
	}
}

// A comment is not SQL. Rewriting identifiers inside one turns a note the
// modeller left for the next reader into something that looks like code, and a
// line comment swallowing a qualified reference changes where the rest of the
// expression thinks its columns live.
func TestScannerSkipsComments(t *testing.T) {
	d := DuckDB{}
	cases := []struct{ in, want string }{
		{"qty -- times price\n* price",
			`"oi"."qty" -- times price` + "\n" + `* "oi"."price"`},
		{"qty /* price is net */ * price",
			`"oi"."qty" /* price is net */ * "oi"."price"`},
		{"/* leading */ amount", `/* leading */ "oi"."amount"`},
		{"amount /* unterminated", `"oi"."amount" /* unterminated`},
		{"a - -b", `"oi"."a" - -"oi"."b"`}, // a lone minus is not a comment
		{"x / y", `"oi"."x" / "oi"."y"`},   // a lone slash is not a comment
	}
	for _, tc := range cases {
		if got := qualifyExpr(tc.in, "oi", d); got != tc.want {
			t.Errorf("qualifyExpr(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
	// And a metric name inside a comment is not a formula reference.
	m := robustModel(t)
	if bad := m.unknownFormulaRefs("rev -- minus shrinkaage\n- 0"); len(bad) > 0 {
		t.Errorf("a word in a comment is not a broken reference: %v", bad)
	}
	if bad := m.unknownFormulaRefs("rev /* shrinkaage */ - 0"); len(bad) > 0 {
		t.Errorf("a word in a block comment is not a broken reference: %v", bad)
	}
}

// A forty-metric domain printed in full is a wall a reader skims and a model
// spends tokens on. A suggestion is always better than a list, so the list is
// the fallback — and bounded when it is reached.
func TestErrorListsAreBounded(t *testing.T) {
	var names []string
	for i := 0; i < 40; i++ {
		names = append(names, fmt.Sprintf("metric_%02d", i))
	}
	got := summarizeNames(names)
	if !strings.HasSuffix(got, fmt.Sprintf("and %d more", 40-maxNamesInError)) {
		t.Errorf("a long list should be truncated, got: %s", got)
	}
	if n := strings.Count(got, ","); n != maxNamesInError-1 {
		t.Errorf("listed %d names, want %d", n+1, maxNamesInError)
	}
	// A short list is printed whole: truncating it would hide the answer.
	short := names[:3]
	if got := summarizeNames(short); got != strings.Join(short, ", ") {
		t.Errorf("a short list should be printed in full, got %q", got)
	}

	// And a near-miss gets the suggestion, not the list — the suggestion is
	// what makes the mistake obvious.
	m := robustModel(t)
	q := Query{Metrics: []string{"revv"}}
	err := m.ResolveMetrics(&q)
	if err == nil || !strings.Contains(err.Error(), "did you mean") {
		t.Errorf("want a suggestion for a near-miss, got: %v", err)
	}
	if strings.Contains(err.Error(), "known:") {
		t.Errorf("a suggestion replaces the list rather than joining it: %v", err)
	}
}

// The mirror of DimensionsFor: pick the breakdown first, and ask which numbers
// can honestly be shown against it. Doing this by asking the forward question
// once per metric costs a round trip each and leaves the caller to intersect
// the results — and a caller that intersects the wrong way offers a metric that
// will be refused when it is finally asked for.
func TestMetricsForBreakdown(t *testing.T) {
	m := shiftsModel(t)

	byRegion, err := m.MetricsFor([]string{"guard_region"}, []string{"finance"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"total_shift_hours", "total_shift_cost", "cost_per_hour"} {
		if !containsName(byRegion, want) {
			t.Errorf("%s conforms onto region and should be offered: %v", want, byRegion)
		}
	}
	if containsName(byRegion, "total_invoiced") {
		t.Errorf("invoicing shares no dimension with shifts: %v", byRegion)
	}

	// Nothing reaches a site attribute without crossing the bridge.
	bySite, err := m.MetricsFor([]string{"client_site_category"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(bySite) != 0 {
		t.Errorf("no measure can be grouped across the bridge: %v", bySite)
	}

	// The intersection is narrower than either dimension alone: pay_component
	// is native to shift_pay, which the hours measure cannot reach.
	both, err := m.MetricsFor([]string{"guard_region", "pay_component"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if containsName(both, "total_shift_hours") {
		t.Errorf("hours cannot be sliced by a pay component: %v", both)
	}
	if !containsName(both, "total_shift_cost") {
		t.Errorf("cost is native to shift_pay and should survive: %v", both)
	}

	// Roles apply here too.
	if got, _ := m.MetricsFor([]string{"client_name"}, nil); containsName(got, "total_invoiced") {
		t.Errorf("a restricted metric must not be offered to a role-less caller: %v", got)
	}

	// An unknown breakdown is an error, not an empty list — "no metric supports
	// this" and "that dimension does not exist" are different answers.
	if _, err := m.MetricsFor([]string{"nope"}, nil); err == nil {
		t.Error("an unknown dimension should be refused")
	}

	// And the report explains each exclusion.
	_, excl, err := m.MetricReport([]string{"client_site_category"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(excl) == 0 {
		t.Fatal("exclusions are what stop a caller concluding the model is incomplete")
	}
	if !strings.Contains(excl[0].Reason, "site_assignment") {
		t.Errorf("the reason should name the offending edge, got %q", excl[0].Reason)
	}

	// ListMetricsBy surfaces the dimension error rather than returning nothing.
	if _, err := m.ListMetricsBy(MetricFilter{Dimensions: []string{"nope"}}); err == nil {
		t.Error("ListMetricsBy should not read an unknown dimension as an empty result")
	}
}
