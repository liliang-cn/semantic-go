package semantic

import (
	"fmt"
	"strings"
)

// Issue is one finding from Lint. Error issues should fail a CI gate; Warn
// issues are advisory (the model still compiles).
type Issue struct {
	Severity string // "error" | "warn"
	Target   string // metric/dimension/entity name
	Message  string
}

func (i Issue) String() string { return fmt.Sprintf("%-5s %s: %s", i.Severity, i.Target, i.Message) }

// Lint is the build-time gate. It enforces two contracts at once:
//
// The GRAIN contract — every dataset states its key tuple, every metric states
// which dataset it aggregates over and with which aggregation, and every metric
// actually compiles to SQL. Validation belongs here, at model-build time, and
// never at query time: the query path should not be the first thing to discover
// that a dataset's grain is a lie.
//
// The METADATA contract — every metric says what it means and offers at least
// one synonym to route to, and any roll-up an agent could get wrong is
// classified. Metadata is the agent's only map; an undescribed metric is an
// invitation to guess.
//
// Returns issues in model order. Callers gate on LintErrors.
func Lint(m *Model) []Issue {
	var out []Issue

	// Routing happens off the description, before any tool call. A domain
	// without one can only be chosen by name, and a name is rarely enough to
	// tell shifts from rostering.
	if m.Description == "" {
		out = append(out, Issue{"warn", firstNonEmptyStr(m.Name, "(model)"),
			"no description — an agent routing between domains has only this line to go on"})
	}

	for i := range m.Entities {
		e := &m.Entities[i]
		switch e.GrainStatus {
		case GrainFail:
			out = append(out, Issue{"error", e.Name, fmt.Sprintf(
				"grain test failed: (%s) is not unique in %s — the dataset must be withheld from the published model until it is fixed",
				e.PrimaryKey, e.Table)})
		case "":
			out = append(out, Issue{"warn", e.Name, fmt.Sprintf(
				"grain (%s) has never been tested for uniqueness — run GrainCheckSQL and record grain_status: pass", e.PrimaryKey)})
		}
	}

	for i := range m.Metrics {
		mt := &m.Metrics[i]
		if mt.Description == "" {
			out = append(out, Issue{"error", mt.Name, "missing description (the agent's only map of what this includes/excludes)"})
		}
		// Aggregation is stated, never inferred from a column name or from the
		// shape of the question that reached it.
		if !mt.IsDerived() && !mt.IsWindow() {
			if mt.Agg == "" {
				out = append(out, Issue{"error", mt.Name, "no aggregation declared: a base metric must state sum|count|count_distinct|avg|min|max"})
			}
			if mt.Expr == "" {
				out = append(out, Issue{"error", mt.Name, "no expr declared: a base metric must state what it aggregates"})
			}
			if mt.Entity == "" {
				out = append(out, Issue{"error", mt.Name, "no native dataset declared: a metric must bind to the dataset it aggregates over, at that dataset's grain"})
			}
		}
		// A bare word in a formula that names no metric is a typo. Unchecked it
		// reaches the warehouse as a column reference — usually an error at run
		// time, from inside generated SQL, long after the model was reviewed.
		if mt.IsDerived() {
			if bad := m.unknownFormulaRefs(mt.Formula); len(bad) > 0 {
				msg := fmt.Sprintf("formula references %s, which name no metric", humanList(bad))
				var sugg []string
				for _, s := range m.SuggestMetricNames(bad[0], 3) {
					if s != mt.Name { // suggesting itself would be a cycle, not a fix
						sugg = append(sugg, s)
					}
				}
				if len(sugg) > 2 {
					sugg = sugg[:2]
				}
				if len(sugg) > 0 {
					msg += fmt.Sprintf(" (did you mean %s?)", humanList(sugg))
				}
				out = append(out, Issue{"error", mt.Name, msg})
			}
		}
		if len(mt.Synonyms) == 0 {
			out = append(out, Issue{"warn", mt.Name, "no synonyms — natural-language asks may not route here"})
		}
		// A metric the layer would infer as non-summable, but that was not
		// declared, is a roll-up trap waiting to happen — ask for it explicitly.
		if mt.Additivity == "" && m.Additivity(mt.Name) == NonAdditive && !mt.IsWindow() {
			out = append(out, Issue{"warn", mt.Name, "inferred non_additive (ratio/distinct) but not declared — set additivity: non_additive"})
		}
		// Does it actually compile? A metric that parses and does not compile
		// is a metric the catalog offers and the query path refuses, which is
		// the worst moment to find out.
		//
		// Skipped when something more specific already fired: the compile error
		// would be the same cause worded worse, and a gate that reports one
		// mistake twice trains people to skim it.
		if !reported(out, mt.Name) {
			if err := compilesUnderANSI(m, mt); err != nil {
				out = append(out, Issue{"error", mt.Name, "does not compile under the ANSI dialect: " + err.Error()})
			}
		}
	}
	return out
}

// compilesUnderANSI proves a metric emits SQL, choosing a group-by that suits
// its shape. A metric is checked in the shape it is MEANT to be asked in: the
// grand total for an ordinary measure, a time series for a window metric, and
// a point in time for a semi-additive one — checking a level at the grand total
// would fail every correctly modelled balance in the catalog, and a gate that
// cries wolf on correct models is one a team turns off.
func compilesUnderANSI(m *Model, mt *Metric) error {
	q := Query{Metrics: []string{mt.Name}, Roles: mt.Roles}
	switch {
	case mt.IsWindow():
		found, err := timeDimensionFor(m, mt.Name, false)
		if err != nil {
			return err
		}
		q.GroupBy = []string{found}
	case m.Additivity(mt.Name) == SemiAdditive:
		found, err := timeDimensionFor(m, mt.Name, true)
		if err != nil {
			return err
		}
		q.GroupBy = []string{found}
	}
	_, err := Compile(m, q, ANSI{})
	return err
}

// timeDimensionFor finds a time dimension the metric can be sliced by. When
// inGrain is set, it must also belong to a base measure's declared key — the
// property that makes one group one snapshot. A derived metric has no entity of
// its own, so the question is asked of the measures it rests on.
func timeDimensionFor(m *Model, metric string, inGrain bool) (string, error) {
	dims, err := m.DimensionsFor(metric)
	if err != nil {
		return "", err
	}
	bases, err := m.baseEntitiesOf(metric, map[string]bool{})
	if err != nil {
		return "", err
	}
	for _, name := range dims {
		d := m.Dimension(name)
		if d == nil || d.Type != "time" {
			continue
		}
		if !inGrain {
			return name, nil
		}
		for _, base := range bases {
			e := m.Entity(base)
			if e != nil && d.Entity == base && inKey(e.PrimaryKey, d.Column) {
				return name, nil
			}
		}
	}
	if inGrain {
		return "", fmt.Errorf("semi_additive metric has no time dimension belonging to the declared grain of %s, "+
			"so no query can pin it to a single snapshot — add one, or reconsider the additivity class",
			strings.Join(bases, "/"))
	}
	return "", fmt.Errorf("window metric has no time dimension reachable from its base measure")
}

// LintErrors returns only the error-severity issues from Lint.
func LintErrors(m *Model) []Issue {
	var errs []Issue
	for _, i := range Lint(m) {
		if i.Severity == "error" {
			errs = append(errs, i)
		}
	}
	return errs
}

// ValidateRefs checks that externally-authored references — the metric and
// dataset names a business-context document claims to be about — still resolve.
//
// The link runs this way round on purpose: the context document declares which
// modelled entities it covers, so curators maintain it without touching the
// data-engineering build, and one document can cover several metrics. The cost
// of that direction is dangling references when a metric is renamed or retired,
// which is exactly what this closes: a stale link fails the gate instead of
// quietly resolving to nothing at lookup time.
func (m *Model) ValidateRefs(metrics, datasets []string) []Issue {
	var out []Issue
	for _, name := range metrics {
		if _, ok := m.ResolveMetricName(name); !ok {
			msg := fmt.Sprintf("references metric %q, which the published model does not define", name)
			if sugg := m.SuggestMetricNames(name, 2); len(sugg) > 0 {
				msg += fmt.Sprintf(" (renamed to %s?)", humanList(sugg))
			}
			out = append(out, Issue{"error", name, msg})
		}
	}
	for _, name := range datasets {
		if m.Entity(name) == nil {
			out = append(out, Issue{"error", name, fmt.Sprintf(
				"references dataset %q, which the published model does not define (known: %s)",
				name, strings.Join(m.EntityNames(), ", "))})
		}
	}
	return out
}

// reported says whether an error has already been raised against a target.
func reported(issues []Issue, target string) bool {
	for _, i := range issues {
		if i.Target == target && i.Severity == "error" {
			return true
		}
	}
	return false
}

func firstNonEmptyStr(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
