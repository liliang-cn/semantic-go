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
		wantIdent    string
		wantTrunc    string
		wantPlace    string
		wantNullSafe string
	}{
		{Postgres{}, `"order_date"`, "date_trunc('month'", "$1", "IS NOT DISTINCT FROM"},
		{Snowflake{}, `"order_date"`, "DATE_TRUNC('month'", ":1", "IS NOT DISTINCT FROM"},
		{Databricks{}, "`order_date`", "date_trunc('MONTH'", "?", "<=>"},
		{DuckDB{}, `"order_date"`, "date_trunc('month'", "$1", "IS NOT DISTINCT FROM"},
		{MySQL{}, "`order_date`", "INTERVAL DAYOFMONTH(", "?", "<=>"},
		{SQLite{}, `"order_date"`, "'start of month'", "?", " IS "},
		{SQLServer{}, "[order_date]", "DATEADD(month, DATEDIFF(month, 0,", "@p1", "IS NULL AND"},
		{BigQuery{}, "`order_date`", "DATE_TRUNC(CAST(", "?", "IS NOT DISTINCT FROM"},
	}
	for _, c := range cases {
		t.Run(c.dialect.Name(), func(t *testing.T) {
			got, err := Compile(m, q, c.dialect)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			if !strings.Contains(got.SQL, c.wantIdent) {
				t.Errorf("%s: identifier not quoted as %s:\n%s", c.dialect.Name(), c.wantIdent, got.SQL)
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

// MySQL has no DATE_TRUNC, so each grain is emulated. Every bucket must come
// back as a DATE — a text label like '2024-10' sorts and compares wrong — and
// weeks must start Monday, matching Postgres, not MySQL's Sunday-default WEEK().
func TestMySQLDateTruncPerGrain(t *testing.T) {
	d := MySQL{}
	for grain, want := range map[string]string{
		"day":     "DATE(ts)",
		"week":    "DATE_SUB(DATE(ts), INTERVAL WEEKDAY(ts) DAY)",
		"month":   "DATE_SUB(DATE(ts), INTERVAL DAYOFMONTH(ts)-1 DAY)",
		"quarter": "DATE_ADD(MAKEDATE(YEAR(ts), 1), INTERVAL QUARTER(ts)-1 QUARTER)",
		"year":    "MAKEDATE(YEAR(ts), 1)",
	} {
		if got := d.DateTrunc(grain, "ts"); got != want {
			t.Errorf("DateTrunc(%q) = %q; want %q", grain, got, want)
		}
	}
	// An unknown grain must produce something the server rejects by name,
	// not a silently-wrong bucket. MySQL has no DATE_TRUNC, so this errors out.
	if got := d.DateTrunc("fortnight", "ts"); !strings.Contains(got, "DATE_TRUNC('fortnight'") {
		t.Errorf("unknown grain should fail loudly, got %q", got)
	}
}

func TestMySQLQuotesWithBackticksAndEscapes(t *testing.T) {
	if got := (MySQL{}).QuoteIdent("order`s"); got != "`order``s`" {
		t.Errorf("QuoteIdent = %q; want %q", got, "`order``s`")
	}
}

func TestDialectByName(t *testing.T) {
	for name, want := range map[string]string{
		"postgres": "postgres", "snowflake": "snowflake",
		"databricks": "databricks", "spark": "databricks", "duckdb": "duckdb",
		"mysql": "mysql", "mariadb": "mysql",
		"sqlite": "sqlite", "sqlite3": "sqlite",
		"sqlserver": "sqlserver", "mssql": "sqlserver", "tsql": "sqlserver",
		"ansi": "ansi",
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

// A time grain that matches no time dimension must fail, not vanish. Silently
// returning day-level rows for a "by month" request is the exact shape of wrong
// answer this layer exists to prevent: correct arithmetic, wrong question.
// SQLite makes this concrete — it has no date type, so a date column arrives as
// TEXT and a generated model calls it categorical.
func TestGrainWithoutTimeDimensionIsRefused(t *testing.T) {
	m := &Model{
		Entities:   []Entity{{Name: "order", Table: "orders", PrimaryKey: StringList{"order_id"}}},
		Dimensions: []Dimension{{Name: "ordered_at", Entity: "order", Column: "ordered_at", Type: "categorical"}},
		Metrics:    []Metric{{Name: "revenue", Description: "d", Entity: "order", Agg: "sum", Expr: "amount"}},
	}
	if err := m.Index(); err != nil {
		t.Fatalf("Index: %v", err)
	}
	_, err := Compile(m, Query{
		Metrics:   []string{"revenue"},
		GroupBy:   []string{"ordered_at"},
		TimeGrain: "month",
	}, Postgres{})
	if err == nil {
		t.Fatal("want an error when the grain matches no time dimension, got nil")
	}
	if !strings.Contains(err.Error(), "time grain") {
		t.Errorf("error should name the problem, got: %v", err)
	}
}

// BigQuery's three traps, each of which produces SQL that either fails to parse
// or returns a plausible wrong number. They are checked on the dialect rather
// than through a compile, because each is a property of one method and a
// compiled query would only show whichever one the fixture happened to reach.
func TestBigQueryAvoidsItsThreeTraps(t *testing.T) {
	bq := BigQuery{}

	// 1. DATE_TRUNC takes the value first and the grain as a keyword, and the
	//    DATE cast is what makes it legal for a TIMESTAMP column — which a
	//    semantic model never states the type of.
	got := bq.DateTrunc("month", "o.order_date")
	if got != "DATE_TRUNC(CAST(o.order_date AS DATE), MONTH)" {
		t.Errorf("date_trunc = %q", got)
	}

	// 2. WEEK starts on Sunday in BigQuery and on Monday everywhere else this
	//    compiler supports. A weekly bucket that means a different week per
	//    engine is the kind of wrong that survives review.
	if got := bq.DateTrunc("week", "x"); !strings.Contains(got, "ISOWEEK") {
		t.Errorf("week grain = %q, want ISOWEEK so the week starts on Monday", got)
	}

	// 3. NUMERIC is precision 38, scale 9. Asking for DECIMAL(38,10) — which
	//    castDecimal38 does, and which six other dialects are happy with — is a
	//    type error here, not a rounding difference.
	dec := bq.CastDecimal("SUM(x)")
	if strings.Contains(dec, "38,10") || strings.Contains(dec, "38, 10") {
		t.Errorf("CastDecimal = %q: BigQuery NUMERIC has scale 9 and refuses 10", dec)
	}
	if !strings.Contains(dec, "NUMERIC") {
		t.Errorf("CastDecimal = %q, want an exact NUMERIC rather than a float", dec)
	}

	// A backtick cannot be inside a BigQuery identifier, and there is no
	// doubling escape: passing one through produces SQL that cannot parse.
	if got := bq.QuoteIdent("we`ird"); got != "`weird`" {
		t.Errorf("QuoteIdent = %q", got)
	}
}

// The name has to resolve, or a client naming their engine in YAML gets the
// Postgres fallback and SQL their warehouse refuses.
func TestBigQueryResolvesByName(t *testing.T) {
	for _, name := range []string{"bigquery", "BigQuery", "bq"} {
		d, ok := DialectByName(name)
		if !ok || d.Name() != "bigquery" {
			t.Errorf("DialectByName(%q) = %v, %v", name, d, ok)
		}
	}
}

// Row limiting is the dialect's to spell. T-SQL has no LIMIT at all — every
// limited query this package emitted for SQL Server used to be a syntax error.
func TestLimitOffsetPerDialect(t *testing.T) {
	cases := []struct {
		d        Dialect
		hasOrder bool
		want     string
	}{
		{Postgres{}, true, "LIMIT 10 OFFSET 20"},
		{BigQuery{}, true, "LIMIT 10 OFFSET 20"},
		{MySQL{}, true, "LIMIT 10 OFFSET 20"},
		{SQLServer{}, true, "OFFSET 20 ROWS FETCH NEXT 10 ROWS ONLY"},
		{SQLServer{}, false, "ORDER BY (SELECT NULL)\nOFFSET 20 ROWS FETCH NEXT 10 ROWS ONLY"},
	}
	for _, tc := range cases {
		if got := tc.d.LimitOffset(10, 20, tc.hasOrder); got != tc.want {
			t.Errorf("%s(hasOrder=%v) = %q, want %q", tc.d.Name(), tc.hasOrder, got, tc.want)
		}
	}
	for _, d := range []Dialect{Postgres{}, SQLServer{}, BigQuery{}} {
		if got := d.LimitOffset(10, 0, true); strings.Contains(got, "LIMIT") && strings.Contains(got, "OFFSET") {
			t.Errorf("%s: a limit with no offset should not emit OFFSET, got %q", d.Name(), got)
		}
	}
}

// An offset with no limit is refused rather than given an invented maximum per
// engine.
func TestOffsetWithoutLimitRefused(t *testing.T) {
	m := shiftsModel(t)
	_, err := Compile(m, Query{Metrics: []string{"total_shift_hours"}, GroupBy: []string{"guard_region"}, Offset: 20}, BigQuery{})
	if err == nil || !strings.Contains(err.Error(), "no limit") {
		t.Fatalf("expected a refusal, got: %v", err)
	}
}
