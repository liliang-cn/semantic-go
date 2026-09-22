package semantic

import (
	"strings"
	"testing"
)

// The spec's worked Stage-4 payload, compiled end to end.
func TestCubeQueryEndToEnd(t *testing.T) {
	m := shiftsModel(t)
	body := []byte(`{
      "measures": ["shift.total_shift_hours", "shift_pay.total_shift_cost"],
      "dimensions": ["shift.shift_type"],
      "filters": [
        {"member": "shift.shift_type", "operator": "equals", "values": ["night"]}
      ],
      "timeDimensions": [
        {"dimension": "shift.shift_date",
         "dateRange": ["2026-08-01", "2026-08-31"],
         "granularity": "month"}
      ]
    }`)
	c, err := CompileCube(m, body, nil, DuckDB{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !strings.Contains(c.SQL, "date_trunc('month'") {
		t.Errorf("granularity lost:\n%s", c.SQL)
	}
	if !strings.Contains(c.SQL, "BETWEEN") {
		t.Errorf("dateRange lost:\n%s", c.SQL)
	}
	if n := strings.Count(c.SQL, " AS (\n  SELECT "); n != 2 {
		t.Errorf("want one CTE per measure, got %d:\n%s", n, c.SQL)
	}
	if got := c.Provenance.TimeGrain; got != "month" {
		t.Errorf("provenance time grain = %q", got)
	}
}

// The §5 scope-boundary payload: a measure filter naming a fact table that is
// never projected.
func TestCubeMeasureFilterAcrossFacts(t *testing.T) {
	m := shiftsModel(t)
	body := []byte(`{
      "measures": ["shift.total_shift_hours"],
      "dimensions": ["guard.guard_region"],
      "dimensionFilters": [
        {"member": "shift.shift_type", "operator": "equals", "values": ["night"]}
      ],
      "measureFilters": [
        {"member": "shift_pay.total_shift_cost", "operator": "gt", "values": ["50000"]}
      ],
      "timeDimensions": [
        {"dimension": "shift.shift_date", "dateRange": ["2026-08-01", "2026-08-31"]}
      ]
    }`)
	c, err := CompileCube(m, body, nil, DuckDB{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !strings.Contains(c.SQL, `WHERE "m_total_shift_cost"."total_shift_cost" > `) {
		t.Errorf("measure filter did not become a post-aggregation predicate:\n%s", c.SQL)
	}
	// "50000" arrives as a string on the wire and must be bound as a number.
	var found bool
	for _, a := range c.Args {
		if n, ok := a.(int64); ok && n == 50000 {
			found = true
		}
	}
	if !found {
		t.Errorf("measure filter value was not converted to a number: %#v", c.Args)
	}
	// No granularity on the timeDimension ⇒ it filters but does not group.
	if containsName(c.Provenance.Dimensions, "shift_date") {
		t.Errorf("a timeDimension without a granularity must not be grouped by: %v", c.Provenance.Dimensions)
	}
}

func TestCubeRefusals(t *testing.T) {
	m := shiftsModel(t)
	cases := []struct{ name, body, want string }{{
		"namespace that contradicts where the metric lives",
		`{"measures": ["guard.total_shift_hours"]}`,
		"belongs to dataset",
	}, {
		"unknown measure gets a suggestion",
		`{"measures": ["shift.total_shift_hour"]}`,
		"did you mean",
	}, {
		"unsupported operator is not approximated",
		`{"measures":["total_shift_hours"],"dimensions":["guard_region"],
		  "filters":[{"member":"shift_type","operator":"matchesRegex","values":["n.*"]}]}`,
		"unsupported operator",
	}, {
		"relative date range has no clock to resolve against",
		`{"measures":["total_shift_hours"],"timeDimensions":[{"dimension":"shift_date","dateRange":"last 7 days"}]}`,
		"no clock or timezone",
	}, {
		"a timezone we cannot honour shifts every boundary",
		`{"measures":["total_shift_hours"],"timezone":"Australia/Sydney"}`,
		"not supported",
	}, {
		"segments are not modelled",
		`{"measures":["total_shift_hours"],"segments":["night_only"]}`,
		"not part of the semantic model",
	}, {
		"mixed and/or group",
		`{"measures":["total_shift_hours"],"dimensions":["guard_region"],
		  "filters":[{"or":[{"member":"shift_type","operator":"equals","values":["night"]},
		                    {"member":"total_shift_cost","operator":"gt","values":["1"]}]}]}`,
		"cannot hold both",
	}, {
		"more sort keys than one ORDER BY can carry",
		`{"measures":["total_shift_hours"],"dimensions":["guard_region"],
		  "order":[["total_shift_hours","desc"],["guard_region","asc"]]}`,
		"single ORDER BY",
	}, {
		"a misspelled key is not a silently unapplied filter",
		`{"measures":["total_shift_hours"],"filterz":[]}`,
		"unknown field",
	}, {
		"a non-numeric measure filter value",
		`{"measures":["total_shift_hours"],"dimensions":["guard_region"],
		  "filters":[{"member":"total_shift_cost","operator":"gt","values":["lots"]}]}`,
		"is not a number",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CompileCube(m, []byte(tc.body), nil, DuckDB{})
			if err == nil {
				t.Fatalf("expected a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want a refusal mentioning %q, got: %v", tc.want, err)
			}
		})
	}
}

func TestCubeOperatorCoverage(t *testing.T) {
	m := shiftsModel(t)
	cases := []struct{ op, want string }{
		{"equals", `= `},
		{"notEquals", `!= `},
		{"contains", `LIKE `},
		{"notContains", `NOT LIKE `},
		{"startsWith", `LIKE `},
		{"endsWith", `LIKE `},
		{"set", `IS NOT NULL`},
		{"notSet", `IS NULL`},
	}
	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			body := `{"measures":["total_shift_hours"],"dimensions":["guard_region"],
			  "filters":[{"member":"shift_type","operator":"` + tc.op + `","values":["night"]}]}`
			c, err := CompileCube(m, []byte(body), nil, DuckDB{})
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			if !strings.Contains(c.SQL, tc.want) {
				t.Errorf("%s did not emit %q:\n%s", tc.op, tc.want, c.SQL)
			}
		})
	}
}

// Nested boolean groups survive the conversion with their structure intact.
func TestCubeNestedFilterGroups(t *testing.T) {
	m := shiftsModel(t)
	body := []byte(`{
      "measures": ["total_shift_hours"],
      "dimensions": ["guard_region"],
      "filters": [{"or": [
        {"member": "shift_type", "operator": "equals", "values": ["night"]},
        {"and": [
          {"member": "shift_type", "operator": "equals", "values": ["day"]},
          {"member": "guard_region", "operator": "equals", "values": ["North"]}
        ]}
      ]}]
    }`)
	c, err := CompileCube(m, body, nil, DuckDB{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	where := c.SQL[strings.Index(c.SQL, "WHERE "):]
	where = where[:strings.Index(where, "\n")]
	if !strings.Contains(where, " OR ") || !strings.Contains(where, " AND ") {
		t.Errorf("nested group flattened:\n%s", where)
	}
	if strings.Count(where, "(") < 2 {
		t.Errorf("group parentheses lost, which changes what the filter means:\n%s", where)
	}
}
