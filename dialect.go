package semantic

import (
	"fmt"
	"strings"
)

// Dialect captures the per-engine SQL differences the compiler needs. It only
// shapes SQL text — it never opens a connection. Postgres ships here;
// Snowflake/Databricks/DuckDB are added the same way.
type Dialect interface {
	Name() string
	QuoteIdent(string) string               // quote a table/column identifier
	DateTrunc(grain, expr string) string    // truncate a date/timestamp to a grain
	Placeholder(i int) string               // bind placeholder for the i-th arg (1-based)
	DistinctFrom(left, right string) string // null-safe equality for outer joins
}

// Postgres dialect.
type Postgres struct{}

func (Postgres) Name() string { return "postgres" }
func (Postgres) QuoteIdent(id string) string {
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}
func (Postgres) DateTrunc(grain, expr string) string {
	return fmt.Sprintf("date_trunc('%s', %s)", grain, expr)
}
func (Postgres) Placeholder(i int) string { return fmt.Sprintf("$%d", i) }
func (Postgres) DistinctFrom(l, r string) string {
	return l + " IS NOT DISTINCT FROM " + r
}

// ANSI is a portable fallback (SQLite/DuckDB-ish): ? placeholders, no date_trunc.
type ANSI struct{}

func (ANSI) Name() string                { return "ansi" }
func (ANSI) QuoteIdent(id string) string { return `"` + strings.ReplaceAll(id, `"`, `""`) + `"` }
func (ANSI) DateTrunc(grain, expr string) string {
	// Best-effort: most engines accept date_trunc; override per real engine.
	return fmt.Sprintf("date_trunc('%s', %s)", grain, expr)
}
func (ANSI) Placeholder(int) string { return "?" }
func (ANSI) DistinctFrom(l, r string) string {
	return "(" + l + " = " + r + " OR (" + l + " IS NULL AND " + r + " IS NULL))"
}

// Snowflake dialect: double-quoted identifiers, positional :N binds, native
// DATE_TRUNC and IS NOT DISTINCT FROM.
type Snowflake struct{}

func (Snowflake) Name() string { return "snowflake" }
func (Snowflake) QuoteIdent(id string) string {
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}
func (Snowflake) DateTrunc(grain, expr string) string {
	return fmt.Sprintf("DATE_TRUNC('%s', %s)", grain, expr)
}
func (Snowflake) Placeholder(i int) string { return fmt.Sprintf(":%d", i) }
func (Snowflake) DistinctFrom(l, r string) string {
	return l + " IS NOT DISTINCT FROM " + r
}

// Databricks (Spark SQL) dialect: backtick-quoted identifiers, ? binds, and the
// null-safe equality operator <=> for outer joins.
type Databricks struct{}

func (Databricks) Name() string { return "databricks" }
func (Databricks) QuoteIdent(id string) string {
	return "`" + strings.ReplaceAll(id, "`", "``") + "`"
}
func (Databricks) DateTrunc(grain, expr string) string {
	// Spark SQL: date_trunc(fmt, ts); fmt is case-insensitive but conventionally upper.
	return fmt.Sprintf("date_trunc('%s', %s)", strings.ToUpper(grain), expr)
}
func (Databricks) Placeholder(int) string { return "?" }
func (Databricks) DistinctFrom(l, r string) string {
	return l + " <=> " + r
}

// DialectByName resolves a dialect by its Name() (case-insensitive). The bool is
// false for an unknown name, so callers can fail loudly instead of guessing.
func DialectByName(name string) (Dialect, bool) {
	switch strings.ToLower(name) {
	case "postgres", "postgresql", "pg", "":
		return Postgres{}, true
	case "snowflake":
		return Snowflake{}, true
	case "databricks", "spark":
		return Databricks{}, true
	case "ansi", "duckdb", "sqlite":
		return ANSI{}, true
	default:
		return nil, false
	}
}
