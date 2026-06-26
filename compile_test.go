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
