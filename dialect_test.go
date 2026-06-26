package semantic

import (
	"strings"
	"testing"
)

// Each dialect must shape the SAME semantic query into engine-correct SQL:
// quoting, placeholders, DATE_TRUNC, and null-safe join equality all differ.
func TestDialectsCompileDistinctSQL(t *testing.T) {
	m := testModel(t) // from compile_test.go: revenue/order_count/aov + windows
	// A grouped, filtered, two-base-metric query exercises quoting, placeholders
	// (in WHERE), date_trunc (time grain), and the null-safe outer join.
	q := Query{
		Metrics:   []string{"revenue", "order_count"},
		GroupBy:   []string{"order_date"},
		TimeGrain: "month",
		Where:     []Filter{{Dimension: "order_date", Op: ">", Values: []any{"2024-01-01"}}},
	}

	cases := []struct {
		dialect      Dialect
		quoteOpen    string
		wantTrunc    string
		wantPlace    string
		wantNullSafe string
	}{
		{Postgres{}, `"`, "date_trunc('month'", "$1", "IS NOT DISTINCT FROM"},
		{Snowflake{}, `"`, "DATE_TRUNC('month'", ":1", "IS NOT DISTINCT FROM"},
		{Databricks{}, "`", "date_trunc('MONTH'", "?", "<=>"},
		{DuckDB{}, `"`, "date_trunc('month'", "$1", "IS NOT DISTINCT FROM"},
	}
	for _, c := range cases {
		t.Run(c.dialect.Name(), func(t *testing.T) {
			got, err := Compile(m, q, c.dialect)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			if !strings.Contains(got.SQL, c.quoteOpen+"order_date"+c.quoteOpen) {
				t.Errorf("%s: identifier not quoted with %q:\n%s", c.dialect.Name(), c.quoteOpen, got.SQL)
			}
			if !strings.Contains(got.SQL, c.wantTrunc) {
				t.Errorf("%s: missing %q:\n%s", c.dialect.Name(), c.wantTrunc, got.SQL)
			}
			if !strings.Contains(got.SQL, c.wantPlace) {
				t.Errorf("%s: missing placeholder %q:\n%s", c.dialect.Name(), c.wantPlace, got.SQL)
			}
			if !strings.Contains(got.SQL, c.wantNullSafe) {
				t.Errorf("%s: missing null-safe join %q:\n%s", c.dialect.Name(), c.wantNullSafe, got.SQL)
			}
			// The filter is applied inside each base-metric CTE (aggregate-then-join),
			// so the bound value appears once per CTE — here two (revenue + order_count).
			if len(got.Args) != 2 {
				t.Errorf("%s: want 2 bound args (one per CTE), got %d", c.dialect.Name(), len(got.Args))
			}
		})
	}
}

func TestDialectByName(t *testing.T) {
	for name, want := range map[string]string{
		"postgres": "postgres", "snowflake": "snowflake",
		"databricks": "databricks", "spark": "databricks", "duckdb": "duckdb",
	} {
		d, ok := DialectByName(name)
		if !ok || d.Name() != want {
			t.Errorf("DialectByName(%q) = %v, %v; want %q", name, d, ok, want)
		}
	}
	if _, ok := DialectByName("oracle"); ok {
		t.Error("unknown dialect should return ok=false")
	}
}
