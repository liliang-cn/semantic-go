package semantic

import (
	"strings"
	"testing"
)

func dbtDomain(t *testing.T) *Model {
	t.Helper()
	domains, err := ImportDBTFile("testdata/dbt/semantic_manifest.json")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(domains) != 1 {
		t.Fatalf("want 1 domain, got %d", len(domains))
	}
	return domains[0].Model
}

func TestImportDBT(t *testing.T) {
	m := dbtDomain(t)

	// The resolved relation is what actually ran, quotes and all — stripped,
	// because this layer quotes identifiers itself per dialect.
	e := m.Entity("shifts")
	if e == nil || e.Table != "pay.public.guard_shifts" {
		t.Fatalf("relation_name not resolved: %+v", e)
	}
	if len(e.PrimaryKey) != 1 || e.PrimaryKey[0] != "shift_id" {
		t.Errorf("grain should come from the primary entity's expr: %v", e.PrimaryKey)
	}

	// A measure becomes an aggregation this layer can reason about, bound to
	// the dataset it lives on.
	mt := m.Metric("total_shift_hours")
	if mt == nil || mt.Agg != "sum" || mt.Entity != "shifts" || mt.Expr != "hours_worked" {
		t.Errorf("simple metric not bound to its measure: %+v", mt)
	}
	if !containsName(mt.Synonyms, "Hours worked") {
		t.Errorf("the dbt label is a synonym worth routing on: %+v", mt.Synonyms)
	}
	if got := m.Additivity("distinct_guards_rostered"); got != NonAdditive {
		t.Errorf("a distinct count is not additive, got %s", got)
	}

	// A ratio guards its denominator: a ratio over a filtered slice reaches a
	// zero sooner or later.
	ratio := m.Metric("cost_per_hour")
	if ratio == nil || !strings.Contains(ratio.Formula, "nullif(total_shift_hours, 0)") {
		t.Errorf("ratio should guard the denominator: %+v", ratio)
	}
	if ratio.Additivity != NonAdditive {
		t.Errorf("a ratio is never summable, got %q", ratio.Additivity)
	}

	// dbt lets a derived metric alias its inputs. The alias must be rewritten
	// back, or the formula references names this layer cannot resolve.
	derived := m.Metric("hours_less_cost_units")
	if derived == nil || derived.Formula != "total_shift_hours - total_shift_cost / 100" {
		t.Errorf("input aliases not resolved: %+v", derived)
	}
	if bad := m.unknownFormulaRefs(derived.Formula); len(bad) > 0 {
		t.Errorf("formula still references non-metrics %v", bad)
	}

	// A cumulative names a MEASURE in dbt and a METRIC here.
	cum := m.Metric("hours_ytd")
	if cum == nil || cum.Of != "total_shift_hours" || cum.Window != "cumulative" || cum.Reset != "year" {
		t.Errorf("cumulative not resolved onto a metric: %+v", cum)
	}

	// The whole import must pass the same gate a hand-written model does.
	if errs := LintErrors(m); len(errs) > 0 {
		t.Errorf("imported model fails the gate: %v", errs)
	}
}

// This is the reason importing from dbt is safer than from the generic
// interchange format: MetricFlow classifies entities, so a join's cardinality is
// stated rather than inferred, and the whole aggregate-then-join guarantee rests
// on exactly that fact.
func TestDBTCardinalityIsStatedNotGuessed(t *testing.T) {
	m := dbtDomain(t)
	if len(m.Joins) != 2 {
		t.Fatalf("want 2 joins from the two foreign entities, got %d: %+v", len(m.Joins), m.Joins)
	}
	for _, j := range m.Joins {
		if j.Cardinality != "many_to_one" {
			t.Errorf("foreign → primary is many-to-one by definition, got %q on %s→%s", j.Cardinality, j.From, j.To)
		}
	}
	// And the guarantee holds through: cost is at pay-component grain, hours at
	// shift grain, and both reach region without multiplying each other.
	c, err := Compile(m, Query{
		Metrics: []string{"total_shift_hours"},
		GroupBy: []string{"region"},
		Where:   []Filter{{Metric: "total_shift_cost", Op: ">", Values: []any{50000}}},
	}, BigQuery{})
	if err != nil {
		t.Fatalf("imported model does not compile: %v", err)
	}
	if n := strings.Count(c.SQL, " AS (\n  SELECT "); n != 2 {
		t.Errorf("each measure should aggregate at its own grain, got %d CTEs:\n%s", n, c.SQL)
	}
}

func TestDBTRefusals(t *testing.T) {
	cases := []struct{ name, doc, want string }{{
		"a dataset with no primary or unique entity has no stated grain",
		`{"semantic_models":[{"name":"a","node_relation":{"relation_name":"t"},
		  "entities":[{"name":"x","type":"foreign"}],"measures":[]}]}`,
		"declares no primary or unique entity",
	}, {
		"an aggregation that cannot be rolled up across a join",
		`{"semantic_models":[{"name":"a","node_relation":{"relation_name":"t"},
		  "entities":[{"name":"a","type":"primary"}],
		  "measures":[{"name":"m","agg":"median","expr":"x"}]}],
		  "metrics":[{"name":"mm","type":"simple","type_params":{"measure":{"name":"m"}}}]}`,
		"median of medians",
	}, {
		"a metric type this layer would have to approximate",
		`{"semantic_models":[{"name":"a","node_relation":{"relation_name":"t"},
		  "entities":[{"name":"a","type":"primary"}],"measures":[]}],
		  "metrics":[{"name":"c","type":"conversion","type_params":{}}]}`,
		"not supported",
	}, {
		"a metric naming a measure nothing declares",
		`{"semantic_models":[{"name":"a","node_relation":{"relation_name":"t"},
		  "entities":[{"name":"a","type":"primary"}],"measures":[]}],
		  "metrics":[{"name":"x","type":"simple","type_params":{"measure":{"name":"ghost"}}}]}`,
		"no semantic model declares",
	}, {
		"the wrong dbt artifact",
		`{"nodes":{}}`,
		"semantic_manifest.json",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ImportDBT([]byte(tc.doc))
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want a refusal mentioning %q, got: %v", tc.want, err)
			}
		})
	}
}

// A measure used only by a cumulative metric is published by no simple metric,
// so the window has nothing to point at unless one is synthesized.
func TestDBTSynthesizesAPrivateCumulativeBase(t *testing.T) {
	doc := `{"semantic_models":[{"name":"s","node_relation":{"relation_name":"t"},
	  "entities":[{"name":"s","type":"primary"}],
	  "dimensions":[{"name":"d","type":"time"}],
	  "measures":[{"name":"private_hours","agg":"sum","expr":"h","description":"x"}]}],
	  "metrics":[{"name":"running","type":"cumulative","description":"x",
	              "type_params":{"measure":{"name":"private_hours"}}}]}`
	domains, err := ImportDBT([]byte(doc))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	m := domains[0].Model
	base := m.Metric("private_hours")
	if base == nil || base.Agg != "sum" || base.Entity != "s" {
		t.Fatalf("a base metric should have been synthesized: %+v", base)
	}
	if got := m.Metric("running").Of; got != "private_hours" {
		t.Errorf("window should rest on the synthesized base, got %q", got)
	}
	if _, err := Compile(m, Query{Metrics: []string{"running"}, GroupBy: []string{"d"}}, DuckDB{}); err != nil {
		t.Errorf("the synthesized base should compile: %v", err)
	}
}

func TestDBTCumulativeWindow(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "cumulative"},
		{"7 days", "rolling:7"},
		{"3 months", "rolling:3"},
	}
	for _, tc := range cases {
		got, err := cumulativeWindow(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("cumulativeWindow(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"days", "0 days", "a lot of days"} {
		if _, err := cumulativeWindow(bad); err == nil {
			t.Errorf("cumulativeWindow(%q) should refuse rather than round", bad)
		}
	}
}

// Renaming an alias must not touch quoted text or qualified references.
func TestRenameIdent(t *testing.T) {
	cases := []struct{ in, want string }{
		{"h - c", "hours - c"},
		{"h.h - h", "h.h - hours"},
		{"'h' || h", "'h' || hours"},
		{"hh + h", "hh + hours"},
	}
	for _, tc := range cases {
		if got := renameIdent(tc.in, "h", "hours"); got != tc.want {
			t.Errorf("renameIdent(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
