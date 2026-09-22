package semantic

import (
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Grain-safety regression set
//
// Every case here is shaped to trigger a fan-out or chasm error if the layer is
// not doing its job, and each is checked three ways:
//
//  1. against an expected answer worked out BY HAND from the fixture — the
//     independent computation, owed nothing to this compiler;
//  2. against a reference query written by hand in plain SQL — a second opinion
//     that shares no code with the compiler;
//  3. where a case has a well-known wrong answer, against the naive query that
//     produces it, asserting the two DIFFER. Without that third check a suite
//     like this can pass while testing nothing, because a trap that was never
//     armed cannot be sprung.
//
// The whole suite runs against a local DuckDB file, so it needs no warehouse,
// no credentials and no network — which is the point: the guarantee is the
// same one the layer claims for a client with no cloud at all.
// ---------------------------------------------------------------------------

type diffCase struct {
	name string
	// query is compiled by this package.
	query Query
	// reference is hand-written SQL believed correct, sharing no code with the
	// compiler.
	reference string
	// want is the answer computed by hand from the fixture.
	want [][]string
	// naive is a plausible query someone would write against a joined-up wide
	// table. When set, the test asserts it disagrees with `want` — proving the
	// trap this case exists to catch is actually present in the fixture.
	naive string
}

func TestGrainSafetyRegressionSet(t *testing.T) {
	db := newDuckDB(t)
	m := shiftsModel(t)

	aug := []any{"2026-08-01", "2026-08-31"}
	night := Filter{Dimension: "shift_type", Op: "=", Values: []any{"night"}}
	inAugust := Filter{Dimension: "shift_date", Op: "between", Values: aug}

	cases := []diffCase{{
		// The scope-boundary case: project hours from one fact, filter by cost
		// from another at a different grain, conformed on region.
		name: "filter-only measure across fact tables at different grains",
		query: Query{
			Metrics: []string{"total_shift_hours"},
			GroupBy: []string{"guard_region"},
			Where:   []Filter{night, inAugust, {Metric: "total_shift_cost", Op: ">", Values: []any{50000}}},
		},
		want: [][]string{{"North", "17"}},
		reference: `
			WITH hours AS (
			  SELECT g.region AS r, SUM(s.hours_worked) AS h
			  FROM guard_shifts s JOIN guards g ON s.guard_id = g.guard_id
			  WHERE s.shift_type = 'night' AND s.shift_date BETWEEN DATE '2026-08-01' AND DATE '2026-08-31'
			  GROUP BY 1),
			cost AS (
			  SELECT g.region AS r, SUM(p.amount) AS c
			  FROM shift_pay p
			  JOIN guard_shifts s ON p.guard_id = s.guard_id AND p.shift_id = s.shift_id
			  JOIN guards g ON s.guard_id = g.guard_id
			  WHERE s.shift_type = 'night' AND s.shift_date BETWEEN DATE '2026-08-01' AND DATE '2026-08-31'
			  GROUP BY 1)
			SELECT hours.r, hours.h FROM hours JOIN cost ON hours.r = cost.r WHERE cost.c > 50000`,
		// The wide-table version: joining pay before aggregating counts g1's
		// 8-hour shift once per pay component.
		naive: `
			SELECT g.region, SUM(s.hours_worked)
			FROM guard_shifts s
			JOIN guards g ON s.guard_id = g.guard_id
			JOIN shift_pay p ON p.guard_id = s.guard_id AND p.shift_id = s.shift_id
			WHERE s.shift_type = 'night' AND s.shift_date BETWEEN DATE '2026-08-01' AND DATE '2026-08-31'
			GROUP BY 1 HAVING SUM(p.amount) > 50000`,
	}, {
		name:  "sum across a one-to-many join does not fan out",
		query: Query{Metrics: []string{"total_shift_hours"}, GroupBy: []string{"guard_region"}},
		want:  [][]string{{"North", "23"}, {"South", "11"}},
		reference: `SELECT g.region, SUM(s.hours_worked)
			FROM guard_shifts s JOIN guards g ON s.guard_id = g.guard_id GROUP BY 1`,
		naive: `SELECT g.region, SUM(s.hours_worked)
			FROM guard_shifts s JOIN guards g ON s.guard_id = g.guard_id
			JOIN shift_pay p ON p.guard_id = s.guard_id AND p.shift_id = s.shift_id GROUP BY 1`,
	}, {
		name:  "two measures at different native grains, both projected",
		query: Query{Metrics: []string{"total_shift_hours", "total_shift_cost"}, GroupBy: []string{"guard_region"}},
		want:  [][]string{{"North", "23", "78000"}, {"South", "11", "1900"}},
		reference: `
			WITH h AS (SELECT g.region r, SUM(s.hours_worked) v FROM guard_shifts s
			             JOIN guards g ON s.guard_id = g.guard_id GROUP BY 1),
			     c AS (SELECT g.region r, SUM(p.amount) v FROM shift_pay p
			             JOIN guard_shifts s ON p.guard_id = s.guard_id AND p.shift_id = s.shift_id
			             JOIN guards g ON s.guard_id = g.guard_id GROUP BY 1)
			SELECT h.r, h.v, c.v FROM h JOIN c ON h.r = c.r`,
	}, {
		name:  "ratio of two measures from different facts",
		query: Query{Metrics: []string{"cost_per_hour"}, GroupBy: []string{"guard_region"}},
		want:  [][]string{{"North", "3391.304347826087"}, {"South", "172.72727272727272"}},
		reference: `
			WITH h AS (SELECT g.region r, SUM(s.hours_worked) v FROM guard_shifts s
			             JOIN guards g ON s.guard_id = g.guard_id GROUP BY 1),
			     c AS (SELECT g.region r, SUM(p.amount) v FROM shift_pay p
			             JOIN guard_shifts s ON p.guard_id = s.guard_id AND p.shift_id = s.shift_id
			             JOIN guards g ON s.guard_id = g.guard_id GROUP BY 1)
			SELECT h.r, c.v / h.v FROM h JOIN c ON h.r = c.r`,
	}, {
		name:  "average is taken at the shift grain, not the pay grain",
		query: Query{Metrics: []string{"avg_shift_hours"}, GroupBy: []string{"guard_region"}},
		want:  [][]string{{"North", "7.666666666666667"}, {"South", "5.5"}},
		reference: `SELECT g.region, AVG(s.hours_worked) FROM guard_shifts s
			JOIN guards g ON s.guard_id = g.guard_id GROUP BY 1`,
		naive: `SELECT g.region, AVG(s.hours_worked) FROM guard_shifts s
			JOIN guards g ON s.guard_id = g.guard_id
			JOIN shift_pay p ON p.guard_id = s.guard_id AND p.shift_id = s.shift_id GROUP BY 1`,
	}, {
		name:  "distinct count over a composite-key fact",
		query: Query{Metrics: []string{"distinct_guards_rostered"}, GroupBy: []string{"guard_region"}},
		want:  [][]string{{"North", "2"}, {"South", "1"}},
		reference: `SELECT g.region, COUNT(DISTINCT s.guard_id) FROM guard_shifts s
			JOIN guards g ON s.guard_id = g.guard_id GROUP BY 1`,
	}, {
		name: "monthly grain with a cumulative window",
		query: Query{
			Metrics:   []string{"total_shift_hours", "hours_cumulative"},
			GroupBy:   []string{"shift_date"},
			TimeGrain: "month",
		},
		want: [][]string{{"2026-08-01", "29", "29"}, {"2026-09-01", "5", "34"}},
		reference: `
			SELECT date_trunc('month', shift_date) d, SUM(hours_worked) v,
			       SUM(SUM(hours_worked)) OVER (ORDER BY date_trunc('month', shift_date) ROWS UNBOUNDED PRECEDING)
			FROM guard_shifts GROUP BY 1`,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Compile(m, tc.query, DuckDB{})
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			got := db.rows(t, inlineArgs(c.SQL, c.Args))

			if !sameRows(got, tc.want) {
				t.Errorf("compiled SQL disagrees with the hand-computed answer\n got: %v\nwant: %v\nSQL:\n%s", got, tc.want, c.SQL)
			}
			if ref := db.rows(t, tc.reference); !sameRows(got, ref) {
				t.Errorf("compiled SQL disagrees with the hand-written reference\n got: %v\n ref: %v\nSQL:\n%s", got, ref, c.SQL)
			}
			if tc.naive != "" {
				if naive := db.rows(t, tc.naive); sameRows(naive, tc.want) {
					t.Errorf("the naive query agrees with the correct answer (%v) — this case is not arming its trap, "+
						"so it would pass even with the semantic layer removed", naive)
				}
			}
		})
	}
}

// Refusals are checked against the same fixture: agreement on a refusal is as
// meaningful as agreement on an answer, and a refusal is only worth anything if
// the query it refuses would really have produced a wrong number.
func TestRefusalsWouldHaveBeenWrong(t *testing.T) {
	db := newDuckDB(t)
	m := shiftsModel(t)

	t.Run("bridge path", func(t *testing.T) {
		if _, err := Compile(m, Query{
			Metrics: []string{"total_shift_hours"},
			GroupBy: []string{"client_site_category"},
		}, DuckDB{}); err == nil {
			t.Fatal("expected a refusal")
		}
		// What the refusal prevented: s1's 8 hours counted under both of the
		// sites it covered, so the total exceeds the 30 hours actually worked.
		rows := db.rows(t, `SELECT SUM(s.hours_worked) FROM guard_shifts s
			JOIN site_assignments a ON a.shift_id = s.shift_id
			JOIN sites si ON si.site_id = a.site_id`)
		total := db.rows(t, `SELECT SUM(hours_worked) FROM guard_shifts`)
		if sameRows(rows, total) {
			t.Fatalf("the bridge does not actually fan out in this fixture (%v vs %v) — the refusal test proves nothing", rows, total)
		}
	})

	t.Run("degenerate having", func(t *testing.T) {
		_, err := Compile(m, Query{
			Metrics: []string{"total_shift_hours"},
			Where:   []Filter{{Metric: "total_shift_cost", Op: ">", Values: []any{50000}}},
		}, DuckDB{})
		if err == nil {
			t.Fatal("expected a refusal")
		}
	})

	t.Run("fact to fact with no conformed dimension", func(t *testing.T) {
		if _, err := Compile(m, Query{
			Metrics: []string{"total_shift_hours", "total_invoiced"},
			GroupBy: []string{"guard_region"},
			Roles:   []string{"finance"},
		}, DuckDB{}); err == nil {
			t.Fatal("expected a refusal")
		}
	})
}

// Every dialect must at least be accepted by an engine that speaks it. DuckDB
// speaks its own and, closely enough, ANSI — so both are parsed for real rather
// than eyeballed as strings.
func TestDialectsParse(t *testing.T) {
	db := newDuckDB(t)
	m := shiftsModel(t)
	q := Query{
		Metrics:   []string{"total_shift_hours", "cost_per_hour"},
		GroupBy:   []string{"guard_region", "shift_date"},
		TimeGrain: "month",
		Where:     []Filter{{Dimension: "shift_type", Op: "in", Values: []any{"night", "day"}}},
	}
	for _, d := range []Dialect{DuckDB{}, ANSI{}} {
		c, err := Compile(m, q, d)
		if err != nil {
			t.Fatalf("%s: compile: %v", d.Name(), err)
		}
		if _, err := db.run(inlineArgs(c.SQL, c.Args)); err != nil {
			t.Errorf("%s: engine rejected the emitted SQL: %v\n%s", d.Name(), err, c.SQL)
		}
	}
}

// The grain gate is executable: the uniqueness assertion this package emits
// must find the duplicate that is really there, and pass on the key that is
// really unique.
func TestGrainCheckSQLAgainstEngine(t *testing.T) {
	db := newDuckDB(t)
	m := shiftsModel(t)

	sql, err := m.GrainCheckSQL("shift", DuckDB{})
	if err != nil {
		t.Fatal(err)
	}
	if rows := db.rows(t, sql); len(rows) != 0 {
		t.Errorf("shift's declared grain (guard_id, shift_id) should be unique, got duplicates: %v", rows)
	}

	// shift_pay keyed only on (guard_id, shift_id) is a lie — g1/s1 has two
	// pay components — and the check must say so rather than shrug.
	m.Entity("shift_pay").PrimaryKey = StringList{"guard_id", "shift_id"}
	sql, err = m.GrainCheckSQL("shift_pay", DuckDB{})
	if err != nil {
		t.Fatal(err)
	}
	rows := db.rows(t, sql)
	if len(rows) != 1 || rows[0][0] != "g1" {
		t.Errorf("the grain check should name g1's duplicated shift, got %v", rows)
	}
}

// --- DuckDB harness --------------------------------------------------------

type duck struct{ path string }

func newDuckDB(t *testing.T) *duck {
	t.Helper()
	if _, err := exec.LookPath("duckdb"); err != nil {
		t.Skip("duckdb CLI not on PATH; skipping the differential suite")
	}
	d := &duck{path: filepath.Join(t.TempDir(), "fixture.duckdb")}
	fixture, err := os.ReadFile("testdata/shifts_fixture.sql")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if _, err := d.run(string(fixture)); err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	return d
}

func (d *duck) run(sql string) (string, error) {
	cmd := exec.Command("duckdb", "-csv", "-noheader", d.path)
	cmd.Stdin = strings.NewReader(sql + ";\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, out)
	}
	// The CLI reports errors on stdout with a zero exit status.
	if s := string(out); strings.Contains(s, "Error:") {
		return "", fmt.Errorf("%s", strings.TrimSpace(s))
	}
	return string(out), nil
}

// rows runs a query and returns its result, sorted, so comparisons do not
// depend on an ORDER BY that the semantic query never asked for.
func (d *duck) rows(t *testing.T, sql string) [][]string {
	t.Helper()
	out, err := d.run(sql)
	if err != nil {
		t.Fatalf("query failed: %v\n%s", err, sql)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil
	}
	recs, err := csv.NewReader(strings.NewReader(out)).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v\n%s", err, out)
	}
	sort.Slice(recs, func(i, j int) bool { return strings.Join(recs[i], "\x00") < strings.Join(recs[j], "\x00") })
	return recs
}

// sameRows compares result sets cell by cell, treating anything that parses as
// a number numerically — 17, 17.0 and 1.7e1 are the same answer, and which one
// an engine prints is not what these cases are testing.
func sameRows(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if !sameCell(a[i][j], b[i][j]) {
				return false
			}
		}
	}
	return true
}

func sameCell(x, y string) bool {
	x, y = strings.TrimSuffix(strings.TrimSpace(x), " 00:00:00"), strings.TrimSuffix(strings.TrimSpace(y), " 00:00:00")
	if x == y {
		return true
	}
	fx, ex := strconv.ParseFloat(strings.TrimSpace(x), 64)
	fy, ey := strconv.ParseFloat(strings.TrimSpace(y), 64)
	if ex != nil || ey != nil {
		return false
	}
	return math.Abs(fx-fy) <= 1e-9*math.Max(1, math.Max(math.Abs(fx), math.Abs(fy)))
}

// inlineArgs substitutes bind arguments into $N placeholders so the CLI, which
// cannot bind, runs exactly the SQL the compiler produced. Test-only: real
// callers pass Compiled.Args to a driver.
func inlineArgs(sql string, args []any) string {
	if strings.Contains(sql, "?") {
		var b strings.Builder
		next := 0
		for _, r := range sql {
			if r == '?' && next < len(args) {
				b.WriteString(sqlLiteral(args[next]))
				next++
				continue
			}
			b.WriteRune(r)
		}
		return b.String()
	}
	for i := len(args); i >= 1; i-- {
		sql = strings.ReplaceAll(sql, fmt.Sprintf("$%d", i), sqlLiteral(args[i-1]))
	}
	return sql
}

func sqlLiteral(v any) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case int, int64, int32:
		return fmt.Sprint(t)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case bool:
		return strings.ToUpper(fmt.Sprint(t))
	default:
		return "'" + strings.ReplaceAll(fmt.Sprint(t), "'", "''") + "'"
	}
}

// Which dialects have execution evidence, and which have only the text of
// their SQL.
//
// TestDialectsParse runs DuckDB and ANSI against a real engine: the SQL is
// parsed and executed, so a mistake in quoting, placeholders or truncation
// fails here rather than in a client's warehouse. The other seven are checked
// by asserting on strings, which catches a wrong function name and cannot
// catch a right one used wrongly.
//
// That is a real difference in how much a dialect is known to work, and it is
// invisible unless something says so. The risk is not the gap — a differential
// suite per warehouse means seven accounts and seven bills — it is that the
// gap goes unrecorded and the next dialect is added believing the suite covers
// it. So every dialect this package resolves must be in exactly one of these
// two lists, and adding one without choosing fails this test.
func TestEveryDialectIsEitherExecutedOrKnownNotToBe(t *testing.T) {
	executed := map[string]bool{"duckdb": true, "ansi": true}
	// Checked as text only: no engine of that kind is reachable from a unit
	// test, so the SQL is asserted on and never run. A client putting one of
	// these into production is the first execution it gets, which is worth
	// knowing when reading a green suite.
	textOnly := map[string]bool{
		"postgres": true, "snowflake": true, "databricks": true,
		"mysql": true, "sqlite": true, "sqlserver": true, "bigquery": true,
	}

	// The names DialectByName resolves are the closed set this must cover.
	// Read back from the resolver rather than typed again here: a list in a
	// test that is not derived from the thing it tests stops being true the
	// day somebody adds a dialect, and passes forever while doing it.
	for _, name := range []string{
		"postgres", "snowflake", "databricks", "duckdb",
		"mysql", "sqlite", "sqlserver", "ansi", "bigquery",
	} {
		d, ok := DialectByName(name)
		if !ok {
			t.Errorf("DialectByName(%q) does not resolve, so this list is stale", name)
			continue
		}
		n := d.Name()
		if executed[n] == textOnly[n] { // in both, or in neither
			t.Errorf("dialect %q is in %d of the two lists, want exactly one: "+
				"say whether its SQL is ever run, or the suite's greenness "+
				"means something different per engine and nothing says so",
				n, map[bool]int{true: 2, false: 0}[executed[n]])
		}
	}
	// And the other direction: a dialect resolvable by a name nobody listed.
	// DialectByName's default is the only guard against a silent addition.
	for _, alias := range []string{"bq", "pg", "postgresql", "mariadb", "sqlite3", "mssql", "tsql", "spark"} {
		d, ok := DialectByName(alias)
		if !ok {
			t.Errorf("alias %q stopped resolving", alias)
			continue
		}
		if !executed[d.Name()] && !textOnly[d.Name()] {
			t.Errorf("alias %q resolves to %q, which is in neither list", alias, d.Name())
		}
	}
}
