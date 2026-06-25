# semantic-go

A small, dependency-light **semantic layer compiler** for Go: declare your
business metrics, dimensions, entities, and join graph once in YAML, then compile
a typed *semantic query* into **fan-out / chasm-safe SQL** for any dialect.

It's the contract between business meaning and SQL — the thing you point an LLM
at instead of 1,200 raw tables, so "revenue" means one governed thing everywhere.

Pure stdlib + `gopkg.in/yaml.v3`. No database, no LLM, no network.

## Why

Valid SQL ≠ correct SQL. Summing an order total *after* joining order line items
multiplies it by the line count — a clean run, a green check, a silently wrong
number. semantic-go makes that class of bug impossible by construction:

- **Three nouns** — every question is a **metric** (what), a **dimension** (how to
  slice), and an **entity** (who/what, with a primary key). Typed, so invalid
  combinations are *refused*, not hallucinated.
- **Declared join graph** — edges carry keys + cardinality. The compiler only
  traverses declared, many-to-one edges; a missing edge fails to compile rather
  than inventing a join. "A refused join is a feature."
- **Aggregate-to-grain-then-join** — each measure is aggregated to the requested
  grain in its own CTE, then the CTEs are outer-joined on shared dimensions. This
  neutralizes both fan-out and chasm traps.

## Install

```sh
go get github.com/liliang-cn/semantic-go
```

## Example

```go
package main

import (
	"fmt"

	semantic "github.com/liliang-cn/semantic-go"
)

func main() {
	m, err := semantic.LoadFile("model.yaml")
	if err != nil {
		panic(err)
	}
	compiled, err := semantic.Compile(m, semantic.Query{
		Metrics: []string{"net_revenue"},
		GroupBy: []string{"store_region"},
	}, semantic.Postgres{})
	if err != nil {
		panic(err) // e.g. "cannot group net_revenue by product_category: not reachable"
	}
	fmt.Println(compiled.SQL)  // run with compiled.Args against your warehouse
}
```

A model is plain YAML — entities (with keys), a join graph (edges with
cardinality), dimensions, and metrics:

```yaml
entities:
  - {name: order,      table: orders,      primary_key: order_id}
  - {name: order_item, table: order_items, primary_key: item_id}

joins:
  - {from: order, to: order_item, cardinality: one_to_many, from_key: order_id, to_key: order_id}

dimensions:
  - {name: store_region, entity: store, column: region, type: categorical}

metrics:
  - name: total_revenue
    entity: order_item
    agg: sum
    expr: "quantity * unit_price"
  - name: net_revenue
    formula: "total_revenue - refund_total"   # derived
  - name: revenue_cumulative
    of: total_revenue
    window: "cumulative"                       # time intelligence
```

## Metric types

- **simple** — `entity` + `agg` (`sum`/`count`/`count_distinct`/`avg`/…) + `expr`
- **ratio / derived** — `formula` over other metric names (computed in the outer SELECT)
- **window / time intelligence** — `of` + `window`: `rolling:N` · `cumulative` · `delta:N` · `prior:N` (require a time dimension in `group_by`)

## API

| Symbol | Purpose |
|---|---|
| `Load([]byte)` / `LoadFile(path)` | parse + validate a model |
| `Compile(*Model, Query, Dialect) (Compiled, error)` | semantic query → SQL + args |
| `Query{Metrics, GroupBy, Where, TimeGrain, OrderBy, Limit}` | the typed intent |
| `Postgres{}` / `ANSI{}` | dialects (`Dialect` interface) |
| `Model.DimensionsFor(metric)` | which dimensions a metric can legally be sliced by |
| `Model.MetricNames()` / `Metric()` / `Dimension()` / `Entity()` | catalog access |

## Status

The compiler covers entities/dimensions/metrics (simple/ratio/derived/window),
the many-to-one join graph, aggregate-then-join, and reachability-based refusal.
Not yet built: bridge (many-to-many) tables, role-playing dimensions,
grain-to-date period reset. Test coverage is light — contributions welcome.

## License

MIT
