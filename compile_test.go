package semantic

import (
	"strings"
	"testing"
)

// testModel is a tiny model exercising the metric shapes M4 cares about:
// an additive base, a non-additive ratio, and window metrics (with/without a
// grain-to-date reset).
func testModel(t *testing.T) *Model {
	t.Helper()
	m := &Model{
		Entities: []Entity{
			{Name: "order_item", Table: "order_items", PrimaryKey: "id"},
			{Name: "order", Table: "orders", PrimaryKey: "order_id"},
		},
		Joins: []Join{
			{From: "order_item", To: "order", FromKey: "order_id", ToKey: "order_id", Cardinality: "many_to_one"},
		},
		Dimensions: []Dimension{
			{Name: "order_date", Entity: "order", Column: "order_date", Type: "time"},
		},
		Metrics: []Metric{
			{Name: "revenue", Description: "d", Synonyms: []string{"sales"}, Entity: "order_item", Agg: "sum", Expr: "qty * price"},
			{Name: "order_count", Description: "d", Synonyms: []string{"orders"}, Entity: "order", Agg: "count_distinct", Expr: "order_id"},
			{Name: "aov", Description: "d", Synonyms: []string{"avg order"}, Formula: "revenue / nullif(order_count, 0)"},
			{Name: "rev_cumulative", Description: "d", Synonyms: []string{"running"}, Of: "revenue", Window: "cumulative"},
			{Name: "rev_ytd", Description: "d", Synonyms: []string{"ytd"}, Of: "revenue", Window: "cumulative", Reset: "year"},
		},
	}
	if err := m.Index(); err != nil {
		t.Fatalf("Index: %v", err)
	}
	return m
}

// Grain-to-date: a reset:year cumulative must PARTITION BY date_trunc('year',…)
// so the running total restarts each year; a plain cumulative must not.
func TestGrainToDateReset(t *testing.T) {
	m := testModel(t)
	q := Query{Metrics: []string{"rev_ytd"}, GroupBy: []string{"order_date"}, TimeGrain: "month"}
	c, err := Compile(m, q, Postgres{})
	if err != nil {
		t.Fatalf("compile rev_ytd: %v", err)
	}
	if !strings.Contains(c.SQL, "PARTITION BY date_trunc('year'") {
		t.Errorf("rev_ytd missing year-reset partition:\n%s", c.SQL)
	}

	q.Metrics = []string{"rev_cumulative"}
	c, err = Compile(m, q, Postgres{})
	if err != nil {
		t.Fatalf("compile rev_cumulative: %v", err)
	}
	if strings.Contains(c.SQL, "PARTITION BY") {
		t.Errorf("plain cumulative must not reset:\n%s", c.SQL)
	}
}

// Additivity: summing a non-additive measure over time must be refused at
// compile time, not silently produce a wrong number.
func TestAdditivityRefusesSummingRatio(t *testing.T) {
	m := testModel(t)
	m.Metrics = append(m.Metrics, Metric{
		Name: "bad_aov_roll", Description: "d", Synonyms: []string{"x"},
		Of: "aov", Window: "cumulative",
	})
	if err := m.Index(); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	q := Query{Metrics: []string{"bad_aov_roll"}, GroupBy: []string{"order_date"}, TimeGrain: "month"}
	_, err := Compile(m, q, Postgres{})
	if err == nil {
		t.Fatal("expected refusal summing a non-additive ratio over time, got nil")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("unexpected error: %v", err)
	}
}

// Additivity inference: ratios and distinct counts are non-additive; plain sums
// are additive.
func TestAdditivityInference(t *testing.T) {
	m := testModel(t)
	cases := map[string]string{
		"revenue":        Additive,
		"order_count":    NonAdditive, // count_distinct
		"aov":            NonAdditive, // ratio formula
		"rev_cumulative": NonAdditive, // window
	}
	for name, want := range cases {
		if got := m.Additivity(name); got != want {
			t.Errorf("Additivity(%q) = %q, want %q", name, got, want)
		}
	}
}

// Lint flags a metric missing its description (the agent's only map).
func TestLintMissingDescription(t *testing.T) {
	m := testModel(t)
	m.Metrics[0].Description = ""
	_ = m.Index()
	errs := LintErrors(m)
	if len(errs) != 1 || errs[0].Target != "revenue" {
		t.Fatalf("expected one description error on revenue, got %+v", errs)
	}
}

// A ratio of two integer measures must not divide as an integer.
//
// SUM() over an integer column returns an integer on Postgres, SQLite and SQL
// Server, and integer division truncates: a defect rate of 2198/149815 comes
// back as 0. The query succeeds, returns a number, and the number is wrong —
// which is the exact failure this layer exists to make impossible, so it is
// worth a test per dialect rather than one for the shape.
func TestIntegerMeasuresDivideAsDecimals(t *testing.T) {
	m := &Model{
		Entities: []Entity{{Name: "inspection", Table: "inspection", PrimaryKey: "id"}},
		Metrics: []Metric{
			{Name: "defects", Entity: "inspection", Agg: "sum", Expr: "defect_qty"},
			{Name: "checked", Entity: "inspection", Agg: "sum", Expr: "checked_qty"},
			{Name: "defect_rate", Formula: "defects / nullif(checked, 0)", Additivity: "non_additive"},
		},
	}
	if err := m.Index(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"postgres", "mysql", "sqlite", "sqlserver", "snowflake", "databricks", "duckdb", "ansi"} {
		d, ok := DialectByName(name)
		if !ok {
			t.Fatalf("%s: no dialect", name)
		}
		got, err := Compile(m, Query{Metrics: []string{"defect_rate"}}, d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// Both operands, not just the numerator: casting one side is enough for
		// the arithmetic but leaves the other in place for a reader to copy.
		if n := strings.Count(got.SQL, "CAST("); n < 2 {
			t.Errorf("%s: %d cast(s), want both operands cast\n%s", name, n, got.SQL)
		}
		if name == "sqlite" && !strings.Contains(got.SQL, "AS REAL") {
			t.Errorf("sqlite: DECIMAL keeps NUMERIC affinity and still divides as an integer\n%s", got.SQL)
		}
	}
}

// A metric selected on its own is not a division and keeps its natural type.
func TestPlainMetricIsNotCast(t *testing.T) {
	m := &Model{
		Entities: []Entity{{Name: "inspection", Table: "inspection", PrimaryKey: "id"}},
		Metrics:  []Metric{{Name: "defects", Entity: "inspection", Agg: "sum", Expr: "defect_qty"}},
	}
	if err := m.Index(); err != nil {
		t.Fatal(err)
	}
	d, _ := DialectByName("postgres")
	got, err := Compile(m, Query{Metrics: []string{"defects"}}, d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.SQL, "CAST(") {
		t.Errorf("plain metric should keep its type:\n%s", got.SQL)
	}
}
