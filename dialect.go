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

// DuckDB dialect: largely Postgres-compatible (double-quoted identifiers,
// positional $N binds, native DATE_TRUNC and IS NOT DISTINCT FROM) — its own type
// so routing and any future divergence stay explicit rather than falling back to
// the generic ANSI shape.
type DuckDB struct{}

func (DuckDB) Name() string { return "duckdb" }
func (DuckDB) QuoteIdent(id string) string {
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}
func (DuckDB) DateTrunc(grain, expr string) string {
	return fmt.Sprintf("date_trunc('%s', %s)", grain, expr)
}
func (DuckDB) Placeholder(i int) string { return fmt.Sprintf("$%d", i) }
func (DuckDB) DistinctFrom(l, r string) string {
	return l + " IS NOT DISTINCT FROM " + r
}

// MySQL dialect (also MariaDB): backtick identifiers, ? binds, and the null-safe
// equality operator <=>.
//
// MySQL has no DATE_TRUNC — not in 8.x, not in MariaDB — so each grain is
// emulated with date arithmetic that returns a DATE. Returning a DATE rather
// than a formatted string matters: a bucket label has to sort chronologically
// and compare against date literals in a WHERE clause, and '2024-10' as text
// does neither.
type MySQL struct{}

func (MySQL) Name() string { return "mysql" }
func (MySQL) QuoteIdent(id string) string {
	return "`" + strings.ReplaceAll(id, "`", "``") + "`"
}

func (MySQL) DateTrunc(grain, expr string) string {
	switch strings.ToLower(grain) {
	case "day":
		return fmt.Sprintf("DATE(%s)", expr)
	case "week":
		// WEEKDAY() is 0 on Monday, so this lands on Monday — the same week
		// start Postgres' date_trunc('week') uses. MySQL's own WEEK() defaults
		// to Sunday, which would silently shift every weekly bucket by a day.
		return fmt.Sprintf("DATE_SUB(DATE(%s), INTERVAL WEEKDAY(%s) DAY)", expr, expr)
	case "month":
		return fmt.Sprintf("DATE_SUB(DATE(%s), INTERVAL DAYOFMONTH(%s)-1 DAY)", expr, expr)
	case "quarter":
		return fmt.Sprintf("DATE_ADD(MAKEDATE(YEAR(%s), 1), INTERVAL QUARTER(%s)-1 QUARTER)", expr, expr)
	case "year":
		return fmt.Sprintf("MAKEDATE(YEAR(%s), 1)", expr)
	default:
		// An unrecognised grain must not silently become "close enough".
		// MySQL has no DATE_TRUNC, so this is a syntax error that names the
		// offending grain — loud, at compile time on the server, rather than a
		// plausible-looking number bucketed the wrong way.
		return fmt.Sprintf("DATE_TRUNC('%s', %s)", grain, expr)
	}
}

func (MySQL) Placeholder(int) string { return "?" }
func (MySQL) DistinctFrom(l, r string) string {
	return l + " <=> " + r
}

// SQLite dialect: double-quoted identifiers, ? binds, and IS as null-safe
// equality.
//
// SQLite has no date_trunc and no date type — dates are text, and the
// modifiers on date() are the only truncation available. Routing "sqlite" to
// the generic ANSI dialect (as this package once did) emitted
// date_trunc('month', …) against an engine with no such function, so every
// time-grained query failed at execution. Buckets come back as 'YYYY-MM-DD'
// text, which is the one text date format that sorts chronologically.
type SQLite struct{}

func (SQLite) Name() string { return "sqlite" }
func (SQLite) QuoteIdent(id string) string {
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}

func (SQLite) DateTrunc(grain, expr string) string {
	switch strings.ToLower(grain) {
	case "day":
		return fmt.Sprintf("date(%s)", expr)
	case "week":
		// strftime('%w') is 0 on Sunday; (%w + 6) %% 7 makes Monday the 0 day,
		// so subtracting it lands on Monday like Postgres' date_trunc('week').
		return fmt.Sprintf("date(%s, '-' || ((CAST(strftime('%%w', %s) AS INTEGER) + 6) %% 7) || ' days')", expr, expr)
	case "month":
		return fmt.Sprintf("date(%s, 'start of month')", expr)
	case "quarter":
		return fmt.Sprintf("date(%s, 'start of year', '+' || (3 * ((CAST(strftime('%%m', %s) AS INTEGER) - 1) / 3)) || ' months')", expr, expr)
	case "year":
		return fmt.Sprintf("date(%s, 'start of year')", expr)
	default:
		return fmt.Sprintf("DATE_TRUNC('%s', %s)", grain, expr) // unknown grain: fail loudly
	}
}

func (SQLite) Placeholder(int) string { return "?" }

// DistinctFrom uses SQLite's IS, which compares NULLs as equal.
func (SQLite) DistinctFrom(l, r string) string { return l + " IS " + r }

// SQLServer (T-SQL) dialect: bracketed identifiers, @pN binds.
//
// DATETRUNC exists only from SQL Server 2022, so truncation uses the
// DATEADD/DATEDIFF idiom that every supported version understands. Its epoch
// (0 = 1900-01-01) is a Monday, so the week grain agrees with Postgres.
// IS NOT DISTINCT FROM is likewise 2022-only, hence the explicit null pair.
type SQLServer struct{}

func (SQLServer) Name() string { return "sqlserver" }
func (SQLServer) QuoteIdent(id string) string {
	return "[" + strings.ReplaceAll(id, "]", "]]") + "]"
}

func (SQLServer) DateTrunc(grain, expr string) string {
	g := strings.ToLower(grain)
	switch g {
	case "day", "week", "month", "quarter", "year":
		return fmt.Sprintf("DATEADD(%s, DATEDIFF(%s, 0, %s), 0)", g, g, expr)
	default:
		return fmt.Sprintf("DATETRUNC(%s, %s)", grain, expr) // unknown grain: fail loudly
	}
}

func (SQLServer) Placeholder(i int) string { return fmt.Sprintf("@p%d", i) }
func (SQLServer) DistinctFrom(l, r string) string {
	return "(" + l + " = " + r + " OR (" + l + " IS NULL AND " + r + " IS NULL))"
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
	case "duckdb":
		return DuckDB{}, true
	case "mysql", "mariadb":
		return MySQL{}, true
	case "sqlite", "sqlite3":
		return SQLite{}, true
	case "sqlserver", "mssql", "tsql":
		return SQLServer{}, true
	case "ansi":
		return ANSI{}, true
	default:
		return nil, false
	}
}
