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

	// CastDecimal makes an expression divide as a decimal rather than as an
	// integer. SUM() over an integer column returns an integer on most engines,
	// and integer ÷ integer truncates: a defect rate of 1.47% comes back as 0.
	// The query runs clean and returns a number, which is the worst way for a
	// metric to be wrong. Engines disagree on how to say it — SQLite's DECIMAL
	// is NUMERIC affinity and still divides as an integer, so it needs REAL.
	CastDecimal(expr string) string

	// LimitOffset renders row limiting. It is only called with limit > 0.
	//
	// hasOrder says whether an ORDER BY was already emitted, because T-SQL's
	// OFFSET/FETCH is legal only after one — a difference worth passing through
	// rather than hiding, since hiding it just moves the syntax error.
	LimitOffset(limit, offset int, hasOrder bool) string
}

// limitOffsetStd is the LIMIT n [OFFSET m] spelling most engines share.
func limitOffsetStd(limit, offset int) string {
	s := fmt.Sprintf("LIMIT %d", limit)
	if offset > 0 {
		s += fmt.Sprintf(" OFFSET %d", offset)
	}
	return s
}

// castDecimal38 is the ANSI spelling most engines accept. 28 integer digits is
// more than any real measure needs, and keeping the scale exact matters: a
// float would make two runs of the same reconciliation disagree in the last
// place, and a control query that fails intermittently gets switched off.
func castDecimal38(expr string) string { return "CAST(" + expr + " AS DECIMAL(38,10))" }

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
func (Postgres) CastDecimal(e string) string         { return "CAST(" + e + " AS numeric)" }
func (Postgres) LimitOffset(l, o int, _ bool) string { return limitOffsetStd(l, o) }

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
func (ANSI) CastDecimal(e string) string         { return castDecimal38(e) }
func (ANSI) LimitOffset(l, o int, _ bool) string { return limitOffsetStd(l, o) }

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
func (Snowflake) CastDecimal(e string) string         { return "CAST(" + e + " AS NUMBER(38,10))" }
func (Snowflake) LimitOffset(l, o int, _ bool) string { return limitOffsetStd(l, o) }

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
func (Databricks) CastDecimal(e string) string         { return castDecimal38(e) }
func (Databricks) LimitOffset(l, o int, _ bool) string { return limitOffsetStd(l, o) }

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
func (DuckDB) CastDecimal(e string) string         { return castDecimal38(e) }
func (DuckDB) LimitOffset(l, o int, _ bool) string { return limitOffsetStd(l, o) }

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
func (MySQL) CastDecimal(e string) string         { return castDecimal38(e) }
func (MySQL) LimitOffset(l, o int, _ bool) string { return limitOffsetStd(l, o) }

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
func (SQLite) CastDecimal(e string) string         { return "CAST(" + e + " AS REAL)" }
func (SQLite) LimitOffset(l, o int, _ bool) string { return limitOffsetStd(l, o) }

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
func (SQLServer) CastDecimal(e string) string { return castDecimal38(e) }

// LimitOffset uses OFFSET/FETCH, the only row-limiting T-SQL has. LIMIT is not
// a T-SQL keyword at all: every limited query this package emitted for SQL
// Server used to be a syntax error on arrival, which is the kind of bug that
// survives precisely because nobody runs the dialect they do not have.
//
// OFFSET/FETCH is legal only after an ORDER BY, so an unordered query gets a
// deterministic no-op one rather than failing.
func (SQLServer) LimitOffset(limit, offset int, hasOrder bool) string {
	s := ""
	if !hasOrder {
		s = "ORDER BY (SELECT NULL)\n"
	}
	return fmt.Sprintf("%sOFFSET %d ROWS FETCH NEXT %d ROWS ONLY", s, offset, limit)
}

// BigQuery dialect: backticked identifiers, ? binds, and three traps that each
// turn a clean run into a wrong number.
//
// The first is DATE_TRUNC's shape. BigQuery takes the value first and the grain
// as a bare keyword — DATE_TRUNC(d, MONTH), not date_trunc('month', d) — and it
// has three functions rather than one: DATE_TRUNC for a DATE, DATETIME_TRUNC
// for a DATETIME, TIMESTAMP_TRUNC for a TIMESTAMP. Handing a TIMESTAMP to
// DATE_TRUNC is an error, so a model whose order_date happens to be a timestamp
// would fail to run under a naive translation. The column's type is not
// something a semantic model states, so the expression is cast to DATE first:
// that is legal from all three types and yields a DATE bucket, which sorts
// chronologically and compares against date literals — the property MySQL's
// entry above explains at length.
//
// The cast is a decision and worth naming: CAST(ts AS DATE) reads a TIMESTAMP in
// UTC. A client whose reporting day is Melbourne's will want their timestamps
// stored or declared accordingly; the alternative — this layer inventing a
// timezone — is how two dashboards come to disagree about what Monday is.
//
// The second is the week. BigQuery's WEEK starts on Sunday; Postgres'
// date_trunc('week') starts on Monday, and MySQL's entry above goes out of its
// way to agree with Postgres. ISOWEEK is BigQuery's Monday-start, so weekly
// buckets mean the same thing on every engine this compiler supports.
//
// The third is the decimal. castDecimal38 asks for DECIMAL(38,10), and
// BigQuery's NUMERIC is fixed at precision 38, scale 9 — a scale of 10 is not a
// rounding difference, it is a type error. NUMERIC unqualified is exact and
// wide enough for any measure; BIGNUMERIC exists for more and costs more to
// store and scan, which is not a trade a metric's division should make on its
// own.
type BigQuery struct{}

func (BigQuery) Name() string { return "bigquery" }

// QuoteIdent backticks the identifier. A backtick cannot appear inside a
// BigQuery identifier at all — there is no doubling escape, as MySQL has — so
// one in the input is removed rather than passed through to produce SQL that
// cannot parse. An identifier containing a backtick names no column in any
// BigQuery table.
func (BigQuery) QuoteIdent(id string) string {
	return "`" + strings.ReplaceAll(id, "`", "") + "`"
}

func (BigQuery) DateTrunc(grain, expr string) string {
	g := strings.ToUpper(grain)
	switch g {
	case "WEEK":
		g = "ISOWEEK" // Monday, to agree with every other dialect here
	case "DAY", "MONTH", "QUARTER", "YEAR":
	default:
		// An unrecognised grain must not silently become "close enough": this
		// is a parse error on the server that names the offending grain.
		return fmt.Sprintf("DATE_TRUNC(CAST(%s AS DATE), %s)", expr, grain)
	}
	return fmt.Sprintf("DATE_TRUNC(CAST(%s AS DATE), %s)", expr, g)
}

// Placeholder is the positional form. BigQuery also has named parameters
// (@name), and they are the better choice for a hand-written query; Compiled
// hands back an ordered []any, and ? is what an ordered list means.
func (BigQuery) Placeholder(int) string { return "?" }

// DistinctFrom uses IS NOT DISTINCT FROM, which BigQuery has had since 2023.
// Unlike SQL Server's, there is no older version still in the field to support:
// the service has one version and everybody is on it.
func (BigQuery) DistinctFrom(l, r string) string {
	return l + " IS NOT DISTINCT FROM " + r
}

// CastDecimal uses NUMERIC — exact, 38 digits, scale 9. See the type comment
// for why DECIMAL(38,10) is not available here.
func (BigQuery) CastDecimal(e string) string { return "CAST(" + e + " AS NUMERIC)" }

func (BigQuery) LimitOffset(l, o int, _ bool) string { return limitOffsetStd(l, o) }

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
	case "bigquery", "bq":
		return BigQuery{}, true
	case "ansi":
		return ANSI{}, true
	default:
		return nil, false
	}
}
