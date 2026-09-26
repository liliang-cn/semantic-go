package semantic

import (
	"strings"
	"testing"
)

// A table named with its schema is quoted part by part. As one identifier —
// "faw_axle.dbt_semcmp.stg_energy_meter" — no engine resolves it, and that is
// exactly what every model imported from a dbt manifest produced: dbt records
// database.schema.table for each semantic model. The first query against a
// real warehouse failed, for every dbt import and every hand-written model
// that named a schema.
func TestASchemaQualifiedTableIsQuotedPartByPart(t *testing.T) {
	m, err := Load([]byte(`
entities:
  - {name: workshop, table: faw_axle.analytics.workshop, primary_key: id}
  - {name: meter, table: public.energy_meter, primary_key: meter_id}
joins:
  - {from: meter, to: workshop, from_key: workshop_id, to_key: id, cardinality: many_to_one}
dimensions:
  - {name: workshop_name, entity: workshop, column: name, type: categorical}
metrics:
  - {name: kwh, description: d, entity: meter, agg: sum, expr: electricity_kwh}
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []Dialect{Postgres{}, BigQuery{}, MySQL{}, SQLServer{}} {
		c, err := Compile(m, Query{Metrics: []string{"kwh"}, GroupBy: []string{"workshop_name"}}, d)
		if err != nil {
			t.Fatalf("%s: %v", d.Name(), err)
		}
		whole := d.QuoteIdent("public.energy_meter")
		if strings.Contains(c.SQL, whole) {
			t.Errorf("%s: the schema-qualified table was quoted as one identifier %s:\n%s", d.Name(), whole, c.SQL)
		}
		want := QuoteTable(d, "faw_axle.analytics.workshop")
		if !strings.Contains(c.SQL, want) {
			t.Errorf("%s: the joined table should read %s:\n%s", d.Name(), want, c.SQL)
		}
	}
	if got := QuoteTable(Postgres{}, "public.orders"); got != `"public"."orders"` {
		t.Errorf("QuoteTable = %s", got)
	}
	if got := QuoteTable(Postgres{}, "orders"); got != `"orders"` {
		t.Errorf("an unqualified table changed: %s", got)
	}
}
