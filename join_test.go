package semantic

import (
	"strings"
	"testing"
)

// Role-playing dimensions: the SAME physical table (stores) is joined twice under
// two roles (sale_store, ship_store) via different foreign keys. The compiler must
// alias each role distinctly so "revenue by sale_region and ship_region" resolves
// both without an ambiguous table reference.
func TestRolePlayingDimensions(t *testing.T) {
	m := &Model{
		Entities: []Entity{
			{Name: "order_item", Table: "order_items", PrimaryKey: "id"},
			{Name: "order", Table: "orders", PrimaryKey: "order_id"},
			{Name: "sale_store", Table: "stores", PrimaryKey: "store_id"},
			{Name: "ship_store", Table: "stores", PrimaryKey: "store_id"},
		},
		Joins: []Join{
			{From: "order_item", To: "order", FromKey: "order_id", ToKey: "order_id", Cardinality: "many_to_one"},
			{From: "order", To: "sale_store", FromKey: "store_id", ToKey: "store_id", Cardinality: "many_to_one"},
			{From: "order", To: "ship_store", FromKey: "ship_store_id", ToKey: "store_id", Cardinality: "many_to_one"},
		},
		Dimensions: []Dimension{
			{Name: "sale_region", Entity: "sale_store", Column: "region", Type: "categorical"},
			{Name: "ship_region", Entity: "ship_store", Column: "region", Type: "categorical"},
		},
		Metrics: []Metric{
			{Name: "revenue", Description: "d", Synonyms: []string{"r"}, Entity: "order_item", Agg: "sum", Expr: "qty*price"},
		},
	}
	if err := m.Index(); err != nil {
		t.Fatalf("index: %v", err)
	}
	c, err := Compile(m, Query{Metrics: []string{"revenue"}, GroupBy: []string{"sale_region", "ship_region"}}, Postgres{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	// Both roles of `stores` must appear, each aliased distinctly and keyed by its
	// own foreign column.
	for _, want := range []string{
		`"stores" AS "sale_store"`,
		`"stores" AS "ship_store"`,
		`"order"."store_id" = "sale_store"."store_id"`,
		`"order"."ship_store_id" = "ship_store"."store_id"`,
		`"sale_store"."region"`,
		`"ship_store"."region"`,
	} {
		if !strings.Contains(c.SQL, want) {
			t.Errorf("missing %q in:\n%s", want, c.SQL)
		}
	}
}

// Bridge (m:n): students ↔ courses via an enrollments bridge (two many-to-one
// edges out of the bridge). A measure on the bridge can be sliced by EITHER side
// without fan-out; a measure on one side may NOT be sliced by the other (no safe
// path) and must be refused.
func bridgeModel(t *testing.T) *Model {
	t.Helper()
	m := &Model{
		Entities: []Entity{
			{Name: "enrollment", Table: "enrollments", PrimaryKey: "id"},
			{Name: "student", Table: "students", PrimaryKey: "student_id"},
			{Name: "course", Table: "courses", PrimaryKey: "course_id"},
		},
		Joins: []Join{
			{From: "enrollment", To: "student", FromKey: "student_id", ToKey: "student_id", Cardinality: "many_to_one"},
			{From: "enrollment", To: "course", FromKey: "course_id", ToKey: "course_id", Cardinality: "many_to_one"},
		},
		Dimensions: []Dimension{
			{Name: "student_name", Entity: "student", Column: "name", Type: "categorical"},
			{Name: "course_title", Entity: "course", Column: "title", Type: "categorical"},
		},
		Metrics: []Metric{
			{Name: "enroll_count", Description: "d", Synonyms: []string{"e"}, Entity: "enrollment", Agg: "count", Expr: "id"},
			{Name: "student_count", Description: "d", Synonyms: []string{"s"}, Entity: "student", Agg: "count", Expr: "student_id"},
		},
	}
	if err := m.Index(); err != nil {
		t.Fatalf("index: %v", err)
	}
	return m
}

func TestBridgeSlicesBothSides(t *testing.T) {
	m := bridgeModel(t)
	c, err := Compile(m, Query{Metrics: []string{"enroll_count"}, GroupBy: []string{"course_title", "student_name"}}, Postgres{})
	if err != nil {
		t.Fatalf("compile through bridge: %v", err)
	}
	for _, want := range []string{
		`"courses" AS "course"`,
		`"students" AS "student"`,
		`"enrollment"."course_id" = "course"."course_id"`,
		`"enrollment"."student_id" = "student"."student_id"`,
	} {
		if !strings.Contains(c.SQL, want) {
			t.Errorf("missing %q in:\n%s", want, c.SQL)
		}
	}
}

func TestBridgeRefusesCrossSideFanout(t *testing.T) {
	m := bridgeModel(t)
	// A student-grain measure cannot be sliced by course: the only path is
	// student → enrollment (one-to-many) → course, which would fan out. Refused.
	_, err := Compile(m, Query{Metrics: []string{"student_count"}, GroupBy: []string{"course_title"}}, Postgres{})
	if err == nil {
		t.Fatal("expected refusal slicing a student measure by course across the bridge")
	}
	if !strings.Contains(err.Error(), "no declared join path") {
		t.Errorf("unexpected error: %v", err)
	}
}
