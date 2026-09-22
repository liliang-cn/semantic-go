# semantic-go

[![CI](https://github.com/liliang-cn/semantic-go/actions/workflows/ci.yml/badge.svg)](https://github.com/liliang-cn/semantic-go/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/liliang-cn/semantic-go.svg)](https://pkg.go.dev/github.com/liliang-cn/semantic-go)
[![Go Report Card](https://goreportcard.com/badge/github.com/liliang-cn/semantic-go)](https://goreportcard.com/report/github.com/liliang-cn/semantic-go)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

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
- **Declared join graph** — edges carry keys (simple or composite) + cardinality.
  The compiler only traverses declared, many-to-one edges; a missing edge fails to
  compile rather than inventing a join. "A refused join is a feature."
- **Declared grain** — every entity states the column tuple that is unique in its
  table, and the layer emits the SQL that proves it. A dataset whose grain test
  failed is withheld from the compiler, not merely flagged.
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
- **window / time intelligence** — `of` + `window`: `rolling:N` · `cumulative` ·
  `delta:N` · `prior:N`, with optional `reset:` for grain-to-date (YTD, QTD)

Each metric may also declare `additivity` (`additive` / `semi_additive` /
`non_additive`), `synonyms`, `roles`, and free-form `meta` tags. Additivity is
inferred when unstated, and it has teeth: a `cumulative` window over a
`count_distinct` is refused, not summed.

### Additivity

Every metric carries a class — `additive`, `semi_additive`, `non_additive` —
declared or inferred, and it is enforced rather than documented:

- A `cumulative` or `rolling:N` window over a non-additive measure (a ratio, a
  distinct count) is refused: re-summing it means nothing.
- A **semi-additive** measure — an inventory level, an account balance, a
  headcount — may be summed across products, regions, anything at all *except*
  time. Over time it must be picked, not added.

Nothing about that second wrong answer looks wrong. Sum a month of daily stock
snapshots and you get roughly thirty times the stock: a large plausible number
in a column of large plausible numbers, moving in the right direction month over
month. So the compiler refuses, and the rule leans on the declared grain:

| Query | |
|---|---|
| grouped by a time dimension **that is part of the measure's key tuple**, ungrained | compiles — one group is one snapshot |
| pinned to one date by an `=` filter | compiles — the sum is across entities, not time |
| any coarser grain, a date range, or no time dimension at all | **refused** |

It refuses rather than repairs, because summing over time correctly needs a
policy nobody has stated — last of period? first? period average? — and each
gives a different number that is defensible under different accounting. Picking
one for the modeller would be inventing the answer.

This is the payoff for making entities declare a key tuple: the model already
says which columns make a row unique, so "is this group a single point in time"
is a question it can answer.

## Filtering: before and after aggregation

`Query.Where` holds one filter tree. A leaf names **either** a dimension or a
metric, and that choice decides which SQL clause it becomes:

| Leaf | Clause | When it runs |
|---|---|---|
| `Dimension` | `WHERE`, inside every measure's CTE | before aggregation |
| `Metric` | applied to the assembled outer query | after aggregation |

```go
semantic.Query{
    Metrics: []string{"total_shift_hours"},
    GroupBy: []string{"guard_region"},
    Where: []semantic.Filter{
        {Dimension: "shift_type", Op: "=", Values: []any{"night"}},
        {Dimension: "shift_date", Op: "between", Values: []any{"2026-08-01", "2026-08-31"}},
        {Metric: "total_shift_cost", Op: ">", Values: []any{50000}},   // ← other fact table
    },
}
```

`total_shift_cost` lives in a different fact table at a different grain and is
never projected. Each measure still aggregates in its own CTE at its own native
grain; both join up to `guard_region`; the cost predicate is applied after.

Three rules make this predictable rather than surprising:

- A filter-only measure can **remove** groups, never add them — the dimension
  spine is built from the projected measures only.
- A filter-only measure is **not** coalesced to zero. A region with no cost rows
  drops out instead of comparing as `0 > 50000`; absence of evidence is not
  evidence.
- A measure filter with **no** `GroupBy` is refused. A `HAVING` on a single grand
  total is ambiguous rather than unsafe, and ambiguity is better rejected than
  resolved by whichever branch happens to run.

Nested groups use `And` / `Or`. A group may not mix the two leaf kinds —
`(region = 'x' OR revenue > 10)` spans two SQL clauses and is refused.

Operators: `=` `!=` `>` `>=` `<` `<=` `in` `not in` `between` `not between`
`contains` `not contains` `starts with` `not starts with` `ends with`
`not ends with` `is null` `is not null`.

A malformed filter is **refused**, not dropped. An operator-less leaf, an `in`
with an empty value list, a `between` with one bound, an operator spelled `~=` —
each of these used to compile to nothing at all, so a query that asked to be
filtered came back unfiltered, with a number larger than it should have been and
entirely plausible. That is the same failure this package exists to prevent,
and it was worse than a fan-out, which at least multiplies by something a reader
might notice. `= NULL` is refused too: it is never true in any engine, and reads
like a filter while behaving like an empty result.

## What compiles, and what is refused

Supported:

- Several fact tables joined through **conformed dimensions** (each fact
  many-to-one onto the shared dimension)
- Measures used **only** for filtering, never projected
- Measures at **differing native grains** in one query
- **Composite** join keys, joined on every column
- **Role-playing** dimensions — declare the same physical table as two entities;
  each is aliased separately, so no new syntax is needed

Refused, with a corrective error:

- Many-to-many paths through bridge tables (no symmetric-aggregate handling)
- Fact-to-fact joins not mediated by a conformed dimension (chasm traps)
- Measure filters with no group-by
- A measure filter on a window metric (a window function cannot sit in `WHERE`)
- Grouping by a dimension the measure cannot reach — the error names the path
  that exists and the edge that makes it unsafe
- Summing a semi-additive measure across time (see **Additivity**)
- A formula naming something that is not a metric — a typo reaches the warehouse
  as a bare column reference otherwise, and is an error at run time at best
- Filtering on a dimension masked for this caller
- A page offset with no page size, which no two engines spell the same way
- A malformed filter, an unknown operator, or the wrong number of values
- The same metric or dimension asked for twice — the result would carry two
  columns under one name
- `order_by` naming something that is not in the result
- At load: two entities, dimensions or metrics sharing a name (one of them could
  never be resolved, and which one would be an accident of file order); one name
  used for both a metric and a dimension; a window metric over another window
  metric (it emits a window function inside another, which no engine accepts)

## Grain contract

Every entity declares its grain, and the layer can prove it:

```go
sql, _ := m.GrainCheckSQL("shift", semantic.DuckDB{})
// SELECT "guard_id", "shift_id", COUNT(*) AS "n_rows"
// FROM "guard_shifts" GROUP BY "guard_id", "shift_id" HAVING COUNT(*) > 1
```

Any row returned is a failure. Record the outcome as `grain_status: pass|fail`
on the entity. A `fail` is not advisory: `Compile` refuses to aggregate over
that dataset *or to join through it*, since a duplicated row on the one-side of
a join fans out just as surely as an undeclared one-to-many edge.

`semc grain` emits the whole suite, so a client with no dbt (and therefore no
dbt CI) can run the same assertion from anything that speaks SQL.

## Domains

A client does not have *a* semantic model. It has a handful — shifts, billing,
rostering — each a governed vocabulary over part of the warehouse, published
separately and owned by different people. Point at a directory and each file
becomes a domain:

```go
cat, _ := semantic.LoadCatalog("models/")   // or one file, or an interchange document
cat.Domains()                               // the routing list: one line each
dom, _ := cat.Get("exec_security_shifts")
```

```yaml
name: exec_security_shifts
description: >-
  Guard shift and pay-rule analytics — hours, headcount and award cost by
  region, guard and shift type.
instructions: >-
  Hours live at shift grain and cost at pay-component grain; ask for both and
  each is aggregated where it lives.
entities: [...]
```

Routing is the one step that should cost no round trip, so `Domains()` returns a
line per domain and no metric names — a client with fifteen domains of forty
metrics would otherwise spend its whole context on a question nobody has asked
yet. `list_metrics` on the chosen domain is the next step.

Two rules, both of them refusals:

- **Naming the domain is required once there is more than one.** Guessing would
  answer a billing question out of the rostering model: a real number about the
  wrong thing, with nothing in the answer to say so.
- **Two domains may not share a name** — one of them could never be routed to,
  and which one would be an accident of load order.

Load order is sorted, so an agent keeping the catalog in context does not see it
reshuffle between runs. A model that names itself beats the file it arrived in,
because files get renamed and copied and a domain routed to yesterday should
still be there today.

## Lookup: what exists, and what may slice it

The catalog opens no database and compiles no SQL. It answers the questions an
agent must settle *before* it commits to a query.

```go
m.ListMetrics(semantic.MetricFilter{
    Roles: []string{"analyst"},
    Meta:  map[string]string{"agent_accessible": "true"},
})
m.DescribeGrain("shift")
avail, unavailable, _ := m.DimensionReport([]string{"total_shift_hours", "total_shift_cost"}, roles)
```

`DimensionReport` takes **all** the metrics a question needs and returns the
intersection — a dimension safe for hours and unsafe for cost is unsafe for a
query asking for both.

The mirror exists too, for a caller that picks the breakdown first:

```go
m.MetricsFor([]string{"guard_region"}, roles)           // which numbers can be shown by region
m.MetricReport([]string{"client_site_category"}, roles) // ...and why the rest cannot
m.ListMetricsBy(semantic.MetricFilter{Dimensions: []string{"guard_region"}})
```

```sh
semc metrics -by guard_region
```

Asking the forward question once per metric gives the same answer, at a round
trip each and with the caller left to intersect the results — and a caller that
intersects the wrong way offers a metric that will be refused when it is finally
asked for. An unknown breakdown is an **error**, not an empty list: "no metric
supports this" and "that dimension does not exist" are different answers.

It also returns `unavailable`: dimensions a reader would reasonably expect to be
groupable, which are not, and the declared edge that excludes them.

```json
{"name": "client_site_category", "dataset": "site",
 "metrics": ["total_shift_hours", "total_shift_cost"],
 "reason": "requires path shift → site_assignment → site, and shift is one-to-many
            onto site_assignment — so aggregating total_shift_hours across it
            would multiply the measure"}
```

Nothing asked for that list, so nothing bounds it by request — it is bounded by
rule instead: within three joins, and excluded *only* for grain-safety reasons,
never merely unrelated. Without it, a missing dimension reads as a gap in the
model, and the reasonable next move is to go around the layer and hand-write the
SQL that the guardrail was correctly preventing.

## Governance

`roles:` on a metric gates resolution, transitively — a public metric whose
formula references a restricted one cannot launder it into view. A caller
presenting no roles is refused, not admitted; restricted metrics are omitted
from the catalog entirely rather than listed and then refused.

`meta:` is not a security boundary. It shapes what is *offered*
(`agent_accessible: "true"`), and `Compile` does not consult it.

A **dimension** can be masked instead of hidden:

```yaml
- name: guard_name
  entity: guard
  column: name
  type: categorical
  roles: [rostering, admin]
  mask: "substr(name, 1, 1) || '.'"
```

Masking is a projection, not a filter: the rows are still there and the measures
over them are still right, so "hours by guard" stays a truthful total while the
names do not leave the warehouse. The mask replaces the column in the `GROUP BY`
as well as the `SELECT` — grouping by the raw column and masking only the output
would leak the distinctness of the hidden values, one row per real person, every
one labelled the same.

A masked dimension **cannot be filtered on** by a caller who cannot see it.
Allowing the predicate while hiding the projection turns the filter into an
oracle: ask for hours where the name starts with `a`, then `b`, and the row
counts read back the value the mask exists to hide. It stays available to group
by. `mask:` without `roles:` and `roles:` without `mask:` are both refused at
load — one hides the value from the people it is meant for, the other leaves
nothing to show the people it is not.

## Cube-shaped query JSON

There is no cross-vendor standard for semantic-layer *queries* — the interchange
formats standardize the model and stop there — so this package also accepts the
JSON shape Cube's REST API uses, because it is the one most widely copied and
therefore the one a language model emits most reliably.

```go
compiled, err := semantic.CompileCube(m, body, roles, semantic.DuckDB{})
```

```json
{
  "measures": ["shift.total_shift_hours"],
  "dimensions": ["guard.guard_region"],
  "filters": [{"member": "shift.shift_type", "operator": "equals", "values": ["night"]}],
  "measureFilters": [{"member": "shift_pay.total_shift_cost", "operator": "gt", "values": ["50000"]}],
  "timeDimensions": [{"dimension": "shift.shift_date",
                      "dateRange": ["2026-08-01", "2026-08-31"], "granularity": "month"}]
}
```

Notes on the conversion, all of them deliberate:

- The `dataset.member` namespace is **checked**, not stripped —
  `orders.total_revenue` naming a metric whose native dataset is `order_item` is
  a real mistake about where a number lives.
- Measure-filter values arrive as strings and are converted to **numbers**.
  Binding `"50000"` as text against a numeric aggregate is engine roulette:
  some coerce, some compare lexically (where `"9" > "50000"`), some error.
- Unknown JSON fields are **rejected**, so a misspelled key is a loud error
  rather than a silently unapplied filter.
- A relative `dateRange` (`"last 7 days"`), a non-UTC `timezone`, `segments`, a
  multi-key `order`, and an unrecognised operator are all **refused**. Each
  would otherwise be honoured approximately, and an approximate filter produces
  an answer that is wrong in a way no reader can see.

Adopting someone else's shape also buys a conformance oracle: the same JSON can
go through Cube and the emitted SQL be diffed against this compiler's.

## Importing an interchange document

```go
domains, err := semantic.ImportOssieFile("osi_document.json")   // or hand-authored YAML
m, err := semantic.OssieDomainNamed(domains, "exec_security_shifts")
```

The interchange document is the **boundary** format, not this compiler's
internal one — import once, validate hard at the door, compile against the model
this package controls. A spec still at 0.x, with fields still in proposal, is a
fine contract to import from and a poor one to ground a compiler's guarantees
on: every round trip through a converter is a chance to lose the one field the
refusals depend on.

Two mismatches are real, and handled explicitly rather than papered over:

**Aggregations are parsed out of the SQL.** An interchange metric carries a whole
expression per dialect (`SUM(shifts.hours_worked)`); this layer stores the
aggregation and the row-level expression separately, because it has to *know*
which aggregation a metric is to reason about additivity and window safety.
`COUNT(DISTINCT x)` becomes `count_distinct`. An expression that is not a single
aggregate call (`SUM(a) - SUM(b)`, `MEDIAN(x)`) is refused — an opaque SQL blob
would still compile and would quietly leave the safety checks nothing to see.
The escape hatch is explicit:

```yaml
custom_extensions:
  semantic_go: {formula: "total_shift_cost / nullif(total_shift_hours, 0)", additivity: non_additive}
```

**Cardinality is inferred only where it is certain.** Relationships are foreign
keys and carry no cardinality, while every refusal here is grounded in it. A key
landing exactly on the target's declared primary key is many-to-one — a fact
about the model, not a guess. Anything else is **refused** with a message saying
what to declare. Import is the worst possible place to introduce a guessed
cardinality, because it is precisely the silent fan-out the layer exists to
prevent.

An expression spanning two datasets is also refused: a base metric aggregates at
exactly one dataset's grain, so `SUM(shifts.hours * rates.multiplier)` has no
single grain to aggregate at. Model it as two metrics and a formula.

## Importing from dbt

A dbt project already states everything this layer needs. Re-typing it into a
second file is how the two drift apart.

```sh
semc import -dbt target/semantic_manifest.json -emit   # review the conversion
semantic-mcp -dbt target/semantic_manifest.json -dialect bigquery
```

```go
domains, err := semantic.ImportDBTFile("target/semantic_manifest.json")
```

What maps to what:

| MetricFlow | here |
|---|---|
| `semantic_models[].node_relation` | the entity's table |
| entities typed `primary` / `unique` | the entity's **grain** (the key tuple) |
| entities typed `foreign` | a **many-to-one** join onto whichever model has that entity as primary |
| `dimensions[]` | dimensions, `time` kept as time |
| `measures[]` + a `simple` metric | a base metric: aggregation and expression, separately |
| `ratio` | a formula, with `nullif` on the denominator and `non_additive` |
| `derived` | a formula, with input aliases rewritten back to metric names |
| `cumulative` | a window metric, `grain_to_date` → `reset:` |

**One thing makes this import safer than the generic interchange one.**
MetricFlow classifies every entity as `primary`, `unique` or `foreign`, so a
join's cardinality is not inferred — it is *stated*. A foreign entity matching a
primary entity elsewhere is many-to-one by definition, and the whole
aggregate-then-join guarantee rests on exactly that fact. Where the interchange
format makes this package refuse and ask, dbt answers.

Refused rather than approximated: a semantic model with no `primary` or `unique`
entity (its grain is unknown); `median` or `percentile` measures (a median of
medians is not a median, so it cannot be rolled up across a join); metric types
this layer would have to fake; a metric naming a measure nothing declares.

> **Read the conversion before you trust it.** This reader was written against
> the documented MetricFlow shape and has **not** been run against a manifest
> from a real dbt build; field names move between dbt versions. It decodes
> leniently — a real manifest carries far more than this — and validates
> strictly, refusing by name anything it cannot map exactly. `-emit` prints the
> model it produced; that is the artifact to review.

`grain_status` is left unset on import: dbt's uniqueness tests live in
`manifest.json`, not the semantic manifest. Run `semc grain` and record the
result, or wire your `dbt test` outcomes in.

## Testing

`go test ./...` runs unit tests plus a **grain-safety regression set** against a
real DuckDB file (`testdata/shifts_fixture.sql`). It skips cleanly when the
`duckdb` CLI is absent, and needs no warehouse, credentials or network — which
is the point: the guarantee is the same one the layer claims for a client with
no cloud at all.

Each case is checked three ways:

1. against an answer computed **by hand** from the fixture;
2. against a **hand-written reference query** sharing no code with the compiler;
3. where a case has a well-known wrong answer, against the **naive** query that
   produces it, asserting the two *differ*.

That third check is not ceremony. Without it a suite like this can pass while
testing nothing, because a trap that was never armed cannot be sprung — and it
caught exactly that here: the fixture's first average case had hours of 8, 7 and
9, where 8 is precisely the mean, so the fan-out was invisible and the test
would have passed with the semantic layer removed.

The same run also proves the emitted SQL parses on a real engine, and that the
grain assertions find a duplicate that is really there.

### Performance

`Index` builds the join graphs and every entity's reachable set once, because
`DimensionReport` — the lookup an agent calls on *every* question — used to
rebuild them once per (dimension × metric × base measure). Over a forty-metric,
twenty-dimension domain:

| | before | after |
|---|---|---|
| `DimensionReport` | 500 µs, 1.4 MB, 9548 allocs | **69 µs, 234 KB, 943 allocs** |
| `Compile` | 30 µs, 643 allocs | 27 µs, 571 allocs |

A metadata lookup costing seventeen times what compiling the actual query cost
was the wrong way round. `bench_test.go` keeps the numbers honest.

A `Model` is read-only after `Index` and safe for concurrent use. Those caches
are shared, so nothing may narrow one in place — `DimensionsFor` used to
intersect the very map it was handed, which one server serving concurrent
requests would have watched shrink as it ran.

## MCP server

`cmd/semantic-mcp` serves a model to an agent over MCP (stdio). It is the
lookup/execution split the whole pattern rests on — three tools that say what
exists and at what grain, one that compiles a structured request — so the agent
picks from a validated menu instead of writing SQL and hoping.

```sh
go install github.com/liliang-cn/semantic-go/cmd/semantic-mcp@latest
semantic-mcp -model models/ -dialect bigquery -roles analyst   # a directory = several domains
semantic-mcp -ossie osi_document.json -domain exec_security_shifts -dialect bigquery
```

| Tool | Returns |
|---|---|
| `list_domains` | the routing catalog — one line per published domain (offered only when there is more than one) |
| `list_metrics` | the whole governed catalog: description, synonyms, aggregation, native dataset, that dataset's grain and its test status. `by: [dims]` narrows it to what those dimensions can slice, with reasons for the rest |
| `list_dimensions` | called with **all** the metrics a question needs → the intersection, plus `unavailable` and the declared join that excludes each one |
| `describe_grain` | a dataset's key tuple and validation status |
| `query_metric` | measures × dimensions × filters → SQL + ordered bind args + provenance |

Four things about it are deliberate:

- **It opens no database.** `query_metric` hands back SQL and arguments for the
  caller's own driver. The same server therefore works against BigQuery, a
  self-hosted Postgres or a local DuckDB file, and the grain guarantees do not
  change with the deployment.
- **A refusal is a successful call whose result says no** (`isError: true` with
  the reason as text), not a JSON-RPC error. "no declared join path from shift
  to site … shift is one-to-many onto site_assignment" tells a model to pick a
  different dimension. Returned as a transport error it reads as a malfunction,
  and the model retries the same call.
- **The gate runs at startup, not per query.** A model failing `Lint` refuses to
  serve, loudly, while somebody is watching — rather than one question at a
  time, in front of a user. `-strict=false` overrides.
- **No SDK.** MCP over stdio is newline-delimited JSON-RPC and four methods;
  writing those keeps this module's dependencies at stdlib plus a YAML parser,
  which is a claim on the first line of this README and one an evaluator checks.
  The handshake, the notification that takes no reply, the strict argument
  decoding and the refusal path are covered by tests, because hand-rolling a
  protocol is only a fair trade if it is actually exercised.

## CLI: `semc`

```sh
go install github.com/liliang-cn/semantic-go/cmd/semc@latest
```

| Command | Does |
|---|---|
| `semc compile` | semantic query → SQL (`-metrics`, `-by`, `-grain`, `-dialect`, `-cube FILE`, `-provenance`) |
| `semc lint` | the build-time gate; exits non-zero on errors |
| `semc grain` | the grain uniqueness assertion for every dataset |
| `semc metrics` | the metric catalog as JSON (`-meta agent_accessible=true`, `-by DIMS`) |
| `semc dimensions` | `-metrics a,b` → what they may be sliced by, and what they may not |
| `semc import` | `-dbt FILE` or `-ossie FILE` → validate an import (`-emit` to review the conversion) |
| `semc domains` | the routing catalog, as JSON |

Every subcommand takes `-domain` when `-model` names a directory.

Common flags: `-model PATH`, `-roles a,b`, `-dialect NAME` (`postgres` `bigquery` `snowflake` `databricks` `duckdb` `mysql` `sqlite` `sqlserver` `ansi`).

```sh
semc compile -model testdata/meridian.yaml -metrics total_revenue -by store_region
semc compile -model testdata/shifts.yaml -cube query.json -dialect duckdb -provenance
semc lint -model testdata/shifts.yaml
semc dimensions -model testdata/shifts.yaml -metrics total_shift_hours,total_shift_cost
```

Compiled query args are printed to stderr as a `-- args:` comment; `-provenance`
adds the chain from answer back to metric definition.

## API

| Symbol | Purpose |
|---|---|
| `Load([]byte)` / `LoadFile(path)` | parse + validate a model |
| `Compile(*Model, Query, Dialect) (Compiled, error)` | semantic query → SQL + args + provenance |
| `CompileCube(*Model, []byte, roles, Dialect)` | Cube-shaped JSON → the same |
| `ImportDBT([]byte)` / `ImportDBTFile(path)` | dbt `semantic_manifest.json` → a domain |
| `ImportOssie([]byte)` / `ImportOssieFile(path)` | interchange document → models, one per domain |
| `Query{Metrics, GroupBy, Where, TimeGrain, OrderBy, Limit, Offset, Roles}` | the typed intent |
| `Filter{Dimension \| Metric, Op, Values, And, Or}` | one predicate, or a group |
| `LoadCatalog(path)` / `NewCatalog(...)` | a file, an interchange document, or a directory → published domains |
| `Catalog.Domains()` / `Get(name)` | the routing list; one domain by name |
| `Model.ListMetrics(MetricFilter)` | the metric catalog |
| `Model.DimensionReport(metrics, roles)` | valid dimensions + why the others are not |
| `Model.MetricsFor(dims, roles)` / `MetricReport` | the mirror: valid metrics for a breakdown + why the others are not |
| `FilterOperators()` | the operator vocabulary, for a tool schema |
| `Model.DescribeGrain(entity)` | declared grain + validation status |
| `Model.GrainCheckSQL` / `GrainCheckSuite` | the uniqueness assertions |
| `Lint(*Model)` / `LintErrors` | the build-time gate |
| `Model.ValidateRefs(metrics, datasets)` | check external references still resolve |
| `Model.Additivity(metric)` | how a measure may be rolled up |
| `Postgres{}` `BigQuery{}` `Snowflake{}` `Databricks{}` `DuckDB{}` `MySQL{}` `SQLite{}` `SQLServer{}` `ANSI{}` | dialects |
| `Model.ListMetrics` + `cmd/semantic-mcp` | the four MCP tools an agent calls |

## Status

Built: entities with declared (composite) grain and a grain gate with teeth;
simple/ratio/derived/window metrics with additivity; the many-to-one join graph
with composite keys; aggregate-then-join across multiple facts at differing
grains; measure filters on unprojected measures; role-playing dimensions;
reachability-based refusal with explanations; role and meta governance;
provenance; nine dialects including BigQuery; Cube-shaped query JSON;
interchange and dbt/MetricFlow import; multi-domain catalogs with routing;
dimension masking; an MCP server; a DuckDB-backed differential suite.

Not built: bridge (many-to-many) tables — symmetric aggregates are not
implemented, and the path is refused rather than approximated; relative date
ranges and timezones (no clock is carried); multi-key `ORDER BY`; a page offset
without a page size, which no two engines spell the same way;
pre-aggregation or materialization, which belong to the execution layer rather
than the compiler.

## License

MIT
