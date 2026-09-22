package semantic

import (
	"strings"
	"testing"
)

func TestImportOssie(t *testing.T) {
	domains, err := ImportOssieFile("testdata/ossie/shifts_ossie.yaml")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(domains) != 1 {
		t.Fatalf("want 1 domain, got %d", len(domains))
	}
	d := domains[0]
	if d.Name != "exec_security_shifts" || !strings.Contains(d.Instructions, "award compliance") {
		t.Errorf("domain metadata lost: %+v", d)
	}
	m := d.Model

	// Datasets become entities, carrying their key tuple and grain status.
	e := m.Entity("shifts")
	if e == nil || e.Table != "pay.public.guard_shifts" {
		t.Fatalf("shifts dataset did not import: %+v", e)
	}
	if len(e.PrimaryKey) != 2 || e.GrainStatus != GrainPass {
		t.Errorf("grain lost on import: %+v", e)
	}

	// The aggregation is split out of the SQL expression, because the safety
	// checks reason about the aggregation, not about an opaque string.
	mt := m.Metric("total_shift_hours")
	if mt == nil || mt.Agg != "sum" || mt.Entity != "shifts" || mt.Expr != "shifts.hours_worked" {
		t.Errorf("expression not split: %+v", mt)
	}
	if got := m.Metric("distinct_guards_rostered"); got.Agg != "count_distinct" || got.Expr != "shifts.guard_id" {
		t.Errorf("COUNT(DISTINCT …) not recognised: %+v", got)
	}
	if got := m.Additivity("distinct_guards_rostered"); got != NonAdditive {
		t.Errorf("a distinct count should be non-additive, got %s", got)
	}
	// The escape hatch carries a metric the parser cannot split.
	if got := m.Metric("cost_per_hour"); got == nil || !got.IsDerived() {
		t.Errorf("custom_extensions metric lost: %+v", got)
	}

	// Foreign keys landing on a declared primary key are many-to-one; nothing
	// was guessed.
	for _, j := range m.Joins {
		if j.Cardinality != "many_to_one" {
			t.Errorf("join %s→%s: want many_to_one, got %q", j.From, j.To, j.Cardinality)
		}
	}
	if len(m.Joins[0].FromKey) != 2 {
		t.Errorf("composite foreign key lost: %+v", m.Joins[0])
	}

	// Only fields with a dimension block become dimensions; hours_worked is a
	// measure input and must not be groupable.
	if m.Dimension("hours_worked") != nil {
		t.Error("a field with no dimension block became a dimension")
	}
	if d := m.Dimension("shift_date"); d == nil || d.Type != "time" {
		t.Errorf("is_time not honoured: %+v", d)
	}
	if d := m.Dimension("guard_name"); d == nil || d.Column != "name" {
		t.Errorf("field column override lost: %+v", d)
	}

	// And the imported model actually compiles, with the same guarantees.
	c, err := Compile(m, Query{
		Metrics: []string{"total_shift_hours"},
		GroupBy: []string{"region"},
		Where:   []Filter{{Metric: "total_shift_cost", Op: ">", Values: []any{50000}}},
	}, DuckDB{})
	if err != nil {
		t.Fatalf("imported model does not compile: %v", err)
	}
	if !strings.Contains(c.SQL, `"shift_pay"."guard_id" = "shifts"."guard_id" AND "shift_pay"."shift_id" = "shifts"."shift_id"`) {
		t.Errorf("composite key not joined in full:\n%s", c.SQL)
	}
}

// An inference that is not certain is refused, because a guessed cardinality is
// exactly the silent fan-out this layer exists to prevent — and import is the
// worst possible place to introduce one.
func TestOssieRefusesUninferableCardinality(t *testing.T) {
	doc := `
semantic_model:
  - name: d
    datasets:
      - {name: a, source: ta, primary_key: [a_id], fields: [{name: x, dimension: {}}]}
      - {name: b, source: tb, primary_key: [b_id], fields: [{name: y, dimension: {}}]}
    relationships:
      - {name: ab, from: {dataset: a, fields: [tag]}, to: {dataset: b, fields: [tag]}}
`
	_, err := ImportOssie([]byte(doc))
	if err == nil {
		t.Fatal("expected a refusal: neither side of the key is a primary key")
	}
	for _, want := range []string{"cannot be inferred", "many_to_one"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should say what to declare, got: %v", err)
		}
	}
}

func TestOssieRefusesMissingGrain(t *testing.T) {
	doc := `
semantic_model:
  - name: d
    datasets:
      - {name: a, source: ta, fields: [{name: x, dimension: {}}]}
`
	_, err := ImportOssie([]byte(doc))
	if err == nil || !strings.Contains(err.Error(), "primary_key") {
		t.Fatalf("a dataset with no declared grain must be refused, got: %v", err)
	}
}

// A base metric aggregates at exactly one dataset's grain. An expression
// spanning two has no single grain to aggregate at, and saying so is more
// useful than picking one.
func TestOssieRefusesCrossDatasetExpression(t *testing.T) {
	doc := `
semantic_model:
  - name: d
    datasets:
      - {name: a, source: ta, primary_key: [a_id]}
      - {name: b, source: tb, primary_key: [b_id]}
    metrics:
      - name: m
        description: x
        expression:
          dialects: [{dialect: ANSI_SQL, expression: "SUM(a.qty * b.rate)"}]
`
	_, err := ImportOssie([]byte(doc))
	if err == nil || !strings.Contains(err.Error(), "spans datasets") {
		t.Fatalf("expected a cross-dataset refusal, got: %v", err)
	}
}

func TestSplitAggregation(t *testing.T) {
	cases := []struct{ in, agg, inner string }{
		{"SUM(shifts.hours_worked)", "sum", "shifts.hours_worked"},
		{"COUNT(DISTINCT shifts.guard_id)", "count_distinct", "shifts.guard_id"},
		{"count(*)", "count", "*"},
		{"AVG(a.x * (b + c))", "avg", "a.x * (b + c)"},
	}
	for _, tc := range cases {
		agg, inner, err := splitAggregation(tc.in)
		if err != nil {
			t.Errorf("%s: %v", tc.in, err)
			continue
		}
		if agg != tc.agg || inner != tc.inner {
			t.Errorf("%s → (%q, %q), want (%q, %q)", tc.in, agg, inner, tc.agg, tc.inner)
		}
	}
	// Two aggregations are not one aggregation, however greedily the shape
	// matches.
	if _, _, err := splitAggregation("SUM(a) - SUM(b)"); err == nil {
		t.Error("expected a refusal for an expression that is two aggregate calls")
	}
	if _, _, err := splitAggregation("MEDIAN(x)"); err == nil {
		t.Error("expected a refusal for an unsupported aggregation")
	}
}

// A document with no ANSI baseline cannot be compiled portably, and the whole
// point of importing is to re-emit per-engine SQL from a portable definition.
func TestOssieRequiresANSIBaseline(t *testing.T) {
	doc := `
semantic_model:
  - name: d
    datasets:
      - {name: a, source: ta, primary_key: [a_id]}
    metrics:
      - name: m
        description: x
        expression:
          dialects:
            - {dialect: BIGQUERY, expression: "SUM(a.x)"}
            - {dialect: SNOWFLAKE, expression: "SUM(a.x)"}
`
	_, err := ImportOssie([]byte(doc))
	if err == nil || !strings.Contains(err.Error(), "ANSI_SQL") {
		t.Fatalf("expected an ANSI baseline to be required, got: %v", err)
	}
}
