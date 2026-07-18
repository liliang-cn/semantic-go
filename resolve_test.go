package semantic

import "testing"

func resolveModel(t *testing.T) *Model {
	t.Helper()
	m := &Model{
		Entities: []Entity{{Name: "sale", Table: "sales", PrimaryKey: "sale_id"}},
		Metrics: []Metric{
			{Name: "revenue", Description: "d", Synonyms: []string{"销售额", "营收", "sales", "income"}, Entity: "sale", Agg: "sum", Expr: "amount"},
			{Name: "units_sold", Description: "d", Synonyms: []string{"销量", "units"}, Entity: "sale", Agg: "sum", Expr: "qty"},
		},
	}
	if err := m.Index(); err != nil {
		t.Fatalf("index: %v", err)
	}
	return m
}

func TestResolveMetricName(t *testing.T) {
	m := resolveModel(t)
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"revenue", "revenue", true}, // canonical, unchanged
		{"营收", "revenue", true},      // CJK synonym
		{"sales", "revenue", true},   // english synonym
		{"Revenue", "revenue", true}, // case-insensitive
		{"销量", "units_sold", true},   // other metric's synonym
		{"profit", "", false},        // unknown
	}
	for _, c := range cases {
		got, ok := m.ResolveMetricName(c.in)
		if ok != c.wantOK || got != c.want {
			t.Errorf("ResolveMetricName(%q) = %q,%v; want %q,%v", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

func TestResolveMetricsRewritesQuery(t *testing.T) {
	m := resolveModel(t)
	q := Query{Metrics: []string{"营收", "units"}}
	if err := m.ResolveMetrics(&q); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(q.Metrics) != 2 || q.Metrics[0] != "revenue" || q.Metrics[1] != "units_sold" {
		t.Fatalf("got %v; want [revenue units_sold]", q.Metrics)
	}
}

func TestResolveMetricsDoesNotMutateCaller(t *testing.T) {
	m := resolveModel(t)
	orig := []string{"营收"}
	q := Query{Metrics: orig}
	if err := m.ResolveMetrics(&q); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if orig[0] != "营收" {
		t.Fatalf("caller slice was mutated: %v", orig)
	}
}

func TestResolveMetricsUnknownSuggests(t *testing.T) {
	m := resolveModel(t)
	q := Query{Metrics: []string{"revenu"}} // typo of revenue
	err := m.ResolveMetrics(&q)
	if err == nil {
		t.Fatal("expected error for unknown metric")
	}
	if got := err.Error(); got == "" ||
		!contains(got, "revenu") || !contains(got, "revenue") {
		t.Fatalf("error should name the input and suggest revenue: %q", got)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
