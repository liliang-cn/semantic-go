package semantic

import "testing"

func TestResolveDimensionSynonym(t *testing.T) {
	m := &Model{
		Entities: []Entity{{Name: "store", Table: "stores", PrimaryKey: "store_id"}},
		Dimensions: []Dimension{{
			Name: "store_region", Entity: "store", Column: "region",
			Type: "categorical", Synonyms: []string{"大区", "区域"},
		}},
	}
	if err := m.Index(); err != nil {
		t.Fatalf("index: %v", err)
	}
	if c, ok := m.ResolveDimensionName("大区"); !ok || c != "store_region" {
		t.Fatalf("大区 -> %q,%v; want store_region,true", c, ok)
	}
	if c, ok := m.ResolveDimensionName("store_region"); !ok || c != "store_region" {
		t.Fatalf("canonical must pass through: %q,%v", c, ok)
	}
	q := Query{GroupBy: []string{"大区", "store_region"}}
	m.ResolveGroupBy(&q)
	if q.GroupBy[0] != "store_region" || q.GroupBy[1] != "store_region" {
		t.Fatalf("group_by resolved to %v; want both store_region", q.GroupBy)
	}
}
