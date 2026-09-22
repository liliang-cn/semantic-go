package main

import (
	"encoding/json"
	"fmt"
	"strings"

	semantic "github.com/liliang-cn/semantic-go"
)

type handler struct {
	catalog *semantic.Catalog
	dialect semantic.Dialect
	filter  semantic.MetricFilter
}

// pick resolves the domain an argument names. When the server publishes one
// domain the argument may be omitted; when it publishes several, omitting it is
// an error rather than a guess — a question about billing answered out of the
// rostering model returns a real number about the wrong thing.
func (h *handler) pick(domain string) (*semantic.Model, error) {
	d, err := h.catalog.Get(domain)
	if err != nil {
		return nil, err
	}
	return d.Model, nil
}

// domainField is the argument every tool below shares. It is described rather
// than required so a single-domain server stays a one-argument call.
func (h *handler) domainField() map[string]any {
	if h.catalog.Len() == 1 {
		return strField("the published domain (optional: this server publishes only " + h.catalog.Names()[0] + ")")
	}
	return strField("which published domain to use — one of: " + strings.Join(h.catalog.Names(), ", "))
}

func (h *handler) register(s *server) {
	if h.catalog.Len() > 1 {
		s.register(toolDef{
			Name: "list_domains",
			Description: "List the published semantic domains, one line each, with what they cover. " +
				"Route to a domain FIRST — every other tool answers about one domain, and asking the wrong " +
				"one wastes a turn at best. A question spanning two domains uses both, one call each.",
			InputSchema: object(map[string]any{}),
			handle:      h.listDomains,
		})
	}

	s.register(toolDef{
		Name: "list_metrics",
		Description: "List the governed metrics available, with their description, synonyms, aggregation, " +
			"native dataset and that dataset's grain. Call this first. It returns the whole catalog rather than " +
			"a relevance-ranked subset: a list that is slightly too long costs tokens, while a retrieval that " +
			"drops the right metric produces a confident answer from the wrong one.",
		InputSchema: object(map[string]any{
			"domain": h.domainField(),
			"search": strField("optional substring to narrow names, descriptions and synonyms"),
			"by": strArray("optional: keep only metrics that can be sliced by EVERY one of these dimensions. " +
				"Use when the breakdown is known first — it is the mirror of list_dimensions, and saves offering " +
				"a metric that would be refused when it is finally asked for"),
		}),
		handle: h.listMetrics,
	})

	s.register(toolDef{
		Name: "list_dimensions",
		Description: "Given ALL the metrics a question needs, return the dimensions they can legally be grouped " +
			"or filtered by — the intersection across every metric, not the union — plus an 'unavailable' list " +
			"naming dimensions that look groupable but are not, and the declared join that excludes them. " +
			"Call with every metric at once: a dimension safe for one measure and unsafe for another is unsafe " +
			"for a query asking for both.",
		InputSchema: object(map[string]any{
			"domain":  h.domainField(),
			"metrics": strArray("metric names, all of the ones the question needs"),
		}, "metrics"),
		handle: h.listDimensions,
	})

	s.register(toolDef{
		Name: "describe_grain",
		Description: "Return a dataset's declared grain (its unique key tuple) and whether the build-time " +
			"uniqueness test has vouched for it. Advisory: query_metric enforces the same rule independently, " +
			"so skipping this call cannot reach an unvalidated dataset — it only saves a wasted turn.",
		InputSchema: object(map[string]any{
			"domain":  h.domainField(),
			"dataset": strField("the dataset (entity) name"),
		}, "dataset"),
		handle: h.describeGrain,
	})

	s.register(toolDef{
		Name: "query_metric",
		Description: "Compile a structured request into grain-safe SQL. Send measures, dimensions and filters " +
			"by name — never SQL, never a table name. Each measure is aggregated at its own native grain first " +
			"and only then joined up to the requested dimensions, so a one-to-many join cannot inflate a sum. " +
			"A filter on a DIMENSION restricts rows before aggregation; a filter on a MEASURE restricts groups " +
			"after it, and may name a measure that is never returned. Requests that cannot be compiled safely " +
			"are refused with a corrective message — read it and pick differently rather than retrying. " +
			"Returns SQL plus ordered bind arguments for your own driver; this server runs nothing.",
		InputSchema: object(map[string]any{
			"domain":     h.domainField(),
			"measures":   strArray("metric names to compute"),
			"dimensions": strArray("dimension names to group by"),
			"filters": map[string]any{
				"type":        "array",
				"description": "Cube-style filters: {member, operator, values}, or {and:[…]} / {or:[…]}. A group may not mix dimension and measure members.",
				"items":       map[string]any{"type": "object"},
			},
			"timeDimensions": map[string]any{
				"type":        "array",
				"description": "{dimension, dateRange:[\"YYYY-MM-DD\",\"YYYY-MM-DD\"], granularity?: day|week|month|quarter|year}. A granularity also groups by it; without one it only filters. Dates must be absolute — this server has no clock.",
				"items":       map[string]any{"type": "object"},
			},
			"order":  strField("a measure or dimension name to sort by"),
			"desc":   map[string]any{"type": "boolean", "description": "sort descending"},
			"limit":  map[string]any{"type": "integer", "description": "row limit"},
			"offset": map[string]any{"type": "integer", "description": "row offset; requires a limit"},
		}, "measures"),
		handle: h.queryMetric,
	})
}

func (h *handler) listDomains(json.RawMessage) (any, error) {
	return map[string]any{
		"domains": h.catalog.Domains(),
		"note": "Pick the domain (or domains) the question is about, then call list_metrics on it. " +
			"Each domain is a separate governed vocabulary; metrics are not comparable across them.",
	}, nil
}

func (h *handler) listMetrics(raw json.RawMessage) (any, error) {
	var args struct {
		Domain string   `json:"domain"`
		Search string   `json:"search"`
		By     []string `json:"by"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	model, err := h.pick(args.Domain)
	if err != nil {
		return nil, err
	}
	filter := h.filter
	filter.Dimensions = args.By
	metrics, err := model.ListMetricsBy(filter)
	if err != nil {
		return nil, err
	}
	if args.Search != "" {
		metrics = filterBySearch(metrics, args.Search)
	}
	out := map[string]any{
		"metrics": metrics,
		"note": "Every metric's aggregation and grain are declared, not inferred. " +
			"Call list_dimensions with the metrics you intend to use before building a query.",
	}
	// When the breakdown was given, say which metrics it rules out and why —
	// the same reason list_dimensions returns its exclusions. A metric missing
	// without explanation reads as a gap in the model.
	if len(args.By) > 0 {
		_, excl, err := model.MetricReport(args.By, h.filter.Roles)
		if err != nil {
			return nil, err
		}
		out["unavailable"] = excl
		out["note"] = "These are the metrics that can be sliced by every dimension you named. " +
			"`unavailable` lists the ones that cannot, and the declared join that prevents it."
	}
	return out, nil
}

func (h *handler) listDimensions(raw json.RawMessage) (any, error) {
	var args struct {
		Domain  string   `json:"domain"`
		Metrics []string `json:"metrics"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	model, err := h.pick(args.Domain)
	if err != nil {
		return nil, err
	}
	if len(args.Metrics) == 0 {
		return nil, fmt.Errorf("metrics is required: pass every metric the question needs, so the intersection is computed once")
	}
	canonical := make([]string, 0, len(args.Metrics))
	for _, name := range args.Metrics {
		c, ok := model.ResolveMetricName(name)
		if !ok {
			if sugg := model.SuggestMetricNames(name, 3); len(sugg) > 0 {
				return nil, fmt.Errorf("unknown metric %q; did you mean %v?", name, sugg)
			}
			return nil, fmt.Errorf("unknown metric %q", name)
		}
		canonical = append(canonical, c)
	}
	avail, excl, err := model.DimensionReport(canonical, h.filter.Roles)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"metrics":     canonical,
		"dimensions":  avail,
		"unavailable": excl,
	}, nil
}

func (h *handler) describeGrain(raw json.RawMessage) (any, error) {
	var args struct {
		Domain  string `json:"domain"`
		Dataset string `json:"dataset"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	model, err := h.pick(args.Domain)
	if err != nil {
		return nil, err
	}
	return model.DescribeGrain(args.Dataset)
}

func (h *handler) queryMetric(raw json.RawMessage) (any, error) {
	// The wire shape is Cube's, minus the fields this layer refuses to
	// approximate. Renaming them into Cube's own spelling keeps one conversion
	// path — and one place where an unsupported request is turned down.
	var args struct {
		Domain         string            `json:"domain"`
		Measures       []string          `json:"measures"`
		Dimensions     []string          `json:"dimensions"`
		Filters        []json.RawMessage `json:"filters"`
		TimeDimensions []json.RawMessage `json:"timeDimensions"`
		Order          string            `json:"order"`
		Desc           bool              `json:"desc"`
		Limit          int               `json:"limit"`
		Offset         int               `json:"offset"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	model, err := h.pick(args.Domain)
	if err != nil {
		return nil, err
	}
	if len(args.Measures) == 0 {
		return nil, fmt.Errorf("measures is required")
	}

	cube := map[string]any{"measures": args.Measures}
	if len(args.Dimensions) > 0 {
		cube["dimensions"] = args.Dimensions
	}
	if len(args.Filters) > 0 {
		cube["filters"] = args.Filters
	}
	if len(args.TimeDimensions) > 0 {
		cube["timeDimensions"] = args.TimeDimensions
	}
	if args.Limit > 0 {
		cube["limit"] = args.Limit
	}
	if args.Offset > 0 {
		cube["offset"] = args.Offset
	}
	if args.Order != "" {
		dir := "asc"
		if args.Desc {
			dir = "desc"
		}
		cube["order"] = [][]string{{args.Order, dir}}
	}
	body, err := json.Marshal(cube)
	if err != nil {
		return nil, err
	}

	compiled, err := semantic.CompileCube(model, body, h.filter.Roles, h.dialect)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"sql":        compiled.SQL,
		"args":       compiled.Args,
		"dialect":    h.dialect.Name(),
		"provenance": compiled.Provenance,
		"note": "Run this against your own warehouse with the args bound positionally. " +
			"provenance gives the chain from each number back to its metric definition.",
	}, nil
}

// filterBySearch narrows the catalog by substring over the fields a question
// would actually match on. It is offered for a domain large enough that the
// whole list is unwieldy; below that, not passing it is the better call.
func filterBySearch(metrics []semantic.MetricInfo, search string) []semantic.MetricInfo {
	q := normalize(search)
	var out []semantic.MetricInfo
	for _, m := range metrics {
		if matches(m, q) {
			out = append(out, m)
		}
	}
	return out
}

func matches(m semantic.MetricInfo, q string) bool {
	if contains(normalize(m.Name), q) || contains(normalize(m.Description), q) {
		return true
	}
	for _, s := range m.Synonyms {
		if contains(normalize(s), q) {
			return true
		}
	}
	return false
}
