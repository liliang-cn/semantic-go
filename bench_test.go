package semantic

import (
	"fmt"
	"testing"
)

// A realistic client domain: a star with a few snowflaked levels.
func bigModel(tb testing.TB, nEntities, nMetrics int) *Model {
	m := &Model{Name: "big", Description: "d"}
	m.Entities = append(m.Entities, Entity{Name: "fact", Table: "f", PrimaryKey: StringList{"id"}, GrainStatus: GrainPass})
	for i := 0; i < nEntities; i++ {
		n := fmt.Sprintf("d%d", i)
		m.Entities = append(m.Entities, Entity{Name: n, Table: "t" + n, PrimaryKey: StringList{"id"}, GrainStatus: GrainPass})
		from := "fact"
		if i > 3 {
			from = fmt.Sprintf("d%d", i-4) // snowflake a few levels deep
		}
		m.Joins = append(m.Joins, Join{From: from, To: n, FromKey: StringList{n + "_id"}, ToKey: StringList{"id"}, Cardinality: "many_to_one"})
		m.Dimensions = append(m.Dimensions, Dimension{Name: "dim" + n, Entity: n, Column: "c", Type: "categorical"})
	}
	m.Dimensions = append(m.Dimensions, Dimension{Name: "fd", Entity: "fact", Column: "d", Type: "time"})
	for i := 0; i < nMetrics; i++ {
		m.Metrics = append(m.Metrics, Metric{
			Name: fmt.Sprintf("m%d", i), Description: "d", Synonyms: []string{fmt.Sprintf("s%d", i)},
			Entity: "fact", Agg: "sum", Expr: "amt",
		})
	}
	if err := m.Index(); err != nil {
		tb.Fatal(err)
	}
	return m
}

func BenchmarkCompile(b *testing.B) {
	m := bigModel(b, 20, 40)
	q := Query{Metrics: []string{"m0", "m1", "m2"}, GroupBy: []string{"dimd0", "dimd9", "fd"}, TimeGrain: "month"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Compile(m, q, DuckDB{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDimensionReport(b *testing.B) {
	m := bigModel(b, 20, 40)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := m.DimensionReport([]string{"m0", "m1", "m2"}, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkListMetrics(b *testing.B) {
	m := bigModel(b, 20, 40)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.ListMetrics(MetricFilter{})
	}
}

func BenchmarkLint(b *testing.B) {
	m := bigModel(b, 20, 40)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Lint(m)
	}
}
