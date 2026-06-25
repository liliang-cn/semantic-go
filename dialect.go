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
