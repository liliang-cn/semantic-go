package semantic

import (
	"strings"
	"testing"
)

const constantMaskModel = `
entities:
  - {name: customer, table: customers, primary_key: id}
  - {name: order, table: orders, primary_key: id}
joins:
  - {from: order, to: customer, from_key: customer_id, to_key: id, cardinality: many_to_one}
dimensions:
  - {name: customer_email, entity: customer, column: email, type: categorical, mask: "'***'", roles: [admin]}
  - {name: customer_initial, entity: customer, column: name, type: categorical, mask: "substr(name, 1, 1)", roles: [admin]}
  - {name: customer_region, entity: customer, column: region, type: categorical}
metrics:
  - {name: revenue, description: d, entity: order, agg: sum, expr: amount}
`

// A constant mask must not reach GROUP BY.
//
// `GROUP BY '***'` is what "revenue by customer email" compiled to for an
// analyst, and Postgres refuses it — "non-integer constant in GROUP BY" — so
// the masked answer was not a masked answer, it was an error. SQLite accepts
// the same SQL, which is why nothing here noticed: the first engine to run it
// was a customer's.
//
// Grouping by a constant was never doing anything. Every row falls in the one
// group either way, and that single '***' row is what masking asks for.
func TestAConstantMaskIsProjectedButNotGroupedBy(t *testing.T) {
	m, err := Load([]byte(constantMaskModel))
	if err != nil {
		t.Fatal(err)
	}
	c, err := Compile(m, Query{Metrics: []string{"revenue"}, GroupBy: []string{"customer_email", "customer_region"}}, Postgres{})
	if err != nil {
		t.Fatalf("a masked dimension must still compile: %v", err)
	}
	for _, line := range strings.Split(c.SQL, "\n") {
		if strings.Contains(line, "GROUP BY") && strings.Contains(line, "'***'") {
			t.Errorf("a constant mask was grouped by: %s\n%s", strings.TrimSpace(line), c.SQL)
		}
	}
	if !strings.Contains(c.SQL, `'***' AS "customer_email"`) {
		t.Errorf("the mask must still be projected:\n%s", c.SQL)
	}
	if !strings.Contains(c.SQL, `GROUP BY "customer"."region"`) {
		t.Errorf("the other dimension must still be grouped by:\n%s", c.SQL)
	}
	if strings.Contains(c.SQL, `"customer"."email"`) {
		t.Errorf("the raw column leaked:\n%s", c.SQL)
	}

	// Alone, it compiles to an aggregate with no GROUP BY at all — one row.
	alone, err := Compile(m, Query{Metrics: []string{"revenue"}, GroupBy: []string{"customer_email"}}, Postgres{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(alone.SQL, "GROUP BY") {
		t.Errorf("grouping only by a constant should leave no GROUP BY:\n%s", alone.SQL)
	}
}

// An expression mask is still grouped by: its groups are the masked values,
// and that is the whole reason the existing test insists on it.
func TestAnExpressionMaskIsStillGroupedBy(t *testing.T) {
	m, err := Load([]byte(constantMaskModel))
	if err != nil {
		t.Fatal(err)
	}
	c, err := Compile(m, Query{Metrics: []string{"revenue"}, GroupBy: []string{"customer_initial"}}, Postgres{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.SQL, `GROUP BY substr("customer"."name", 1, 1)`) {
		t.Errorf("an expression mask must be grouped by:\n%s", c.SQL)
	}
}

// A caller holding the role groups by the real column, as before.
func TestACallerWhoMaySeeTheColumnStillGroupsByIt(t *testing.T) {
	m, err := Load([]byte(constantMaskModel))
	if err != nil {
		t.Fatal(err)
	}
	c, err := Compile(m, Query{Metrics: []string{"revenue"}, GroupBy: []string{"customer_email"}, Roles: []string{"admin"}}, Postgres{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.SQL, `GROUP BY "customer"."email"`) {
		t.Errorf("admin should group by the raw email:\n%s", c.SQL)
	}
}

func TestWhichMasksAreConstants(t *testing.T) {
	for _, s := range []string{`'***'`, `'REDACTED'`, `'it''s'`, `NULL`, `null`, `0`, `-1.5`, `('***')`, `'***'::text`, `NULL::varchar(64)`, `true`} {
		if !constantExpr(s) {
			t.Errorf("%s should be a constant", s)
		}
	}
	for _, s := range []string{`substr(name, 1, 1)`, `left(email, 2) || '***'`, `name`, `'***' || id`, `md5(email)`, `CASE WHEN x THEN 'a' ELSE 'b' END`} {
		if constantExpr(s) {
			t.Errorf("%s references a column and must be grouped by", s)
		}
	}
}
