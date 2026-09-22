package semantic

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ---------------------------------------------------------------------------
// dbt / MetricFlow import
//
// A dbt project already states everything this layer needs, in
// semantic_manifest.json: which table a semantic model sits on, which column
// tuple identifies a row, which columns are dimensions, and what each measure
// aggregates. Re-typing that into a second file is how the two drift apart.
//
// One thing makes this import safer than the generic interchange one. MetricFlow
// classifies every entity as `primary`, `unique` or `foreign`, so a join's
// cardinality is not inferred — it is *stated*. A foreign entity in one model
// matching a primary entity in another is many-to-one by definition, and the
// whole aggregate-then-join guarantee rests on exactly that fact. Where the
// interchange format made this package refuse and ask, dbt answers.
//
// IMPORTANT, and please read it before trusting a conversion: this reader was
// written against the documented MetricFlow shape and has NOT been run against a
// manifest from a real dbt build. Field names move between dbt versions. It
// therefore decodes leniently (unknown fields are ignored — a real manifest
// carries far more than this) and validates strictly (anything it cannot map
// exactly is refused by name, never guessed at). Run
//
//	semc import -dbt target/semantic_manifest.json -emit
//
// and read the model it produces before pointing anything at a warehouse. A
// silent misreading here would be a wrong number, which is the one outcome this
// package exists to prevent.
// ---------------------------------------------------------------------------

type dbtManifest struct {
	SemanticModels []dbtSemanticModel `json:"semantic_models"`
	Metrics        []dbtMetric        `json:"metrics"`
	ProjectName    string             `json:"project_name"`
}

type dbtSemanticModel struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	NodeRelation dbtNodeRelation `json:"node_relation"`
	Entities     []dbtEntity     `json:"entities"`
	Dimensions   []dbtDimension  `json:"dimensions"`
	Measures     []dbtMeasure    `json:"measures"`
}

// table prefers the fully qualified relation dbt resolved, falling back to the
// alias. The relation name is what actually ran, so it is what this layer emits.
func (sm dbtSemanticModel) table() string {
	if sm.NodeRelation.RelationName != "" {
		return strings.ReplaceAll(sm.NodeRelation.RelationName, `"`, "")
	}
	parts := make([]string, 0, 3)
	for _, p := range []string{sm.NodeRelation.Database, sm.NodeRelation.SchemaName, sm.NodeRelation.Alias} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ".")
}

type dbtNodeRelation struct {
	Alias        string `json:"alias"`
	SchemaName   string `json:"schema_name"`
	Database     string `json:"database"`
	RelationName string `json:"relation_name"`
}

type dbtEntity struct {
	Name string `json:"name"`
	Type string `json:"type"` // primary | unique | foreign | natural
	Expr string `json:"expr"`
	Role string `json:"role"`
}

// column is the physical column an entity or dimension maps to: its expr when
// one is given, otherwise its name.
func (e dbtEntity) column() string { return firstNonEmpty(e.Expr, e.Name) }

type dbtDimension struct {
	Name        string            `json:"name"`
	Type        string            `json:"type"` // categorical | time
	Description string            `json:"description"`
	Expr        string            `json:"expr"`
	TypeParams  *dbtDimTypeParams `json:"type_params"`
}

func (d dbtDimension) column() string { return firstNonEmpty(d.Expr, d.Name) }

type dbtDimTypeParams struct {
	TimeGranularity string `json:"time_granularity"`
}

type dbtMeasure struct {
	Name        string `json:"name"`
	Agg         string `json:"agg"`
	Expr        string `json:"expr"`
	Description string `json:"description"`
}

func (m dbtMeasure) column() string { return firstNonEmpty(m.Expr, m.Name) }

type dbtMetric struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Type        string            `json:"type"` // simple | ratio | derived | cumulative | conversion
	Label       string            `json:"label"`
	TypeParams  dbtMetricParams   `json:"type_params"`
	Meta        map[string]string `json:"meta"`
}

type dbtMetricParams struct {
	Measure     *dbtMetricInput  `json:"measure"`
	Numerator   *dbtMetricInput  `json:"numerator"`
	Denominator *dbtMetricInput  `json:"denominator"`
	Expr        string           `json:"expr"`
	Metrics     []dbtMetricInput `json:"metrics"`
	Window      string           `json:"window"`
	GrainToDate string           `json:"grain_to_date"`
	// cumulative metrics moved their fields under cumulative_type_params in
	// later dbt versions; both spellings are read.
	Cumulative *dbtCumulativeParams `json:"cumulative_type_params"`
}

type dbtCumulativeParams struct {
	Window      string `json:"window"`
	GrainToDate string `json:"grain_to_date"`
	PeriodAgg   string `json:"period_agg"`
}

type dbtMetricInput struct {
	Name  string `json:"name"`
	Alias string `json:"alias"`
}

// ImportDBT reads a dbt semantic_manifest.json and returns it as one domain.
func ImportDBT(data []byte) ([]Domain, error) {
	var man dbtManifest
	if err := json.Unmarshal(data, &man); err != nil {
		return nil, fmt.Errorf("parse semantic_manifest.json: %w", err)
	}
	if len(man.SemanticModels) == 0 {
		return nil, fmt.Errorf("manifest declares no semantic_models: this is the MetricFlow artifact " +
			"(target/semantic_manifest.json), not target/manifest.json")
	}
	m := &Model{Name: firstNonEmpty(man.ProjectName, "dbt")}

	// Datasets, their grain, and their dimensions.
	byModel := map[string]*dbtSemanticModel{}
	primaryOf := map[string]string{} // entity name → the dataset it is primary in
	for i := range man.SemanticModels {
		sm := &man.SemanticModels[i]
		if sm.Name == "" {
			return nil, fmt.Errorf("a semantic model has no name")
		}
		if sm.table() == "" {
			return nil, fmt.Errorf("semantic model %q resolves to no table (node_relation is empty)", sm.Name)
		}
		byModel[sm.Name] = sm

		var key []string
		for _, e := range sm.Entities {
			switch strings.ToLower(e.Type) {
			case "primary", "unique":
				key = append(key, e.column())
				primaryOf[e.Name] = sm.Name
			}
		}
		if len(key) == 0 {
			return nil, fmt.Errorf("semantic model %q declares no primary or unique entity, so its grain is unknown: "+
				"every dataset must state the column tuple that makes a row unique before it can be aggregated over", sm.Name)
		}
		m.Entities = append(m.Entities, Entity{
			Name:       sm.Name,
			Table:      sm.table(),
			PrimaryKey: key,
			Grain:      sm.Description,
		})
		for _, d := range sm.Dimensions {
			typ := "categorical"
			if strings.EqualFold(d.Type, "time") {
				typ = "time"
			}
			m.Dimensions = append(m.Dimensions, Dimension{
				Name:   d.Name,
				Entity: sm.Name,
				Column: d.column(),
				Type:   typ,
			})
		}
	}

	// Joins. A foreign entity here whose name is primary there is many-to-one,
	// by MetricFlow's own definition — this is the one place an importer gets
	// cardinality as a fact rather than an inference.
	for i := range man.SemanticModels {
		sm := &man.SemanticModels[i]
		for _, e := range sm.Entities {
			if !strings.EqualFold(e.Type, "foreign") {
				continue
			}
			target, ok := primaryOf[e.Name]
			if !ok || target == sm.Name {
				// A foreign key to nothing published is not a join this layer
				// can traverse; it is simply absent, and a query needing it
				// gets the usual "no declared join path" refusal.
				continue
			}
			t := byModel[target]
			m.Joins = append(m.Joins, Join{
				From:        sm.Name,
				To:          target,
				FromKey:     StringList{e.column()},
				ToKey:       StringList{primaryColumn(t, e.Name)},
				Cardinality: "many_to_one",
			})
		}
	}

	// Metrics.
	measureOwner := map[string]*dbtSemanticModel{}
	measureDef := map[string]dbtMeasure{}
	for i := range man.SemanticModels {
		for _, ms := range man.SemanticModels[i].Measures {
			measureOwner[ms.Name] = &man.SemanticModels[i]
			measureDef[ms.Name] = ms
		}
	}
	for _, dm := range man.Metrics {
		converted, err := convertDBTMetric(dm, measureOwner, measureDef)
		if err != nil {
			return nil, fmt.Errorf("metric %q: %w", dm.Name, err)
		}
		m.Metrics = append(m.Metrics, converted)
	}

	// A cumulative metric in dbt names a MEASURE; here a window metric wraps a
	// METRIC. Usually a simple metric already publishes that measure and the
	// window can point at it. When none does, the measure is private to the
	// cumulative, and a base metric is synthesized so the window has something
	// to rest on — otherwise the import would drop the metric, or keep it
	// pointing at a name that resolves to nothing.
	if err := resolveCumulativeBases(m, measureOwner, measureDef); err != nil {
		return nil, err
	}

	if err := m.Index(); err != nil {
		return nil, err
	}
	return []Domain{{Name: m.Name, Description: m.Description, Model: m}}, nil
}

// ImportDBTFile reads and converts a dbt semantic manifest from disk.
func ImportDBTFile(path string) ([]Domain, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ImportDBT(data)
}

// resolveCumulativeBases rewrites each window metric's `of` from a measure name
// to a metric name, adding a base metric where none exists.
func resolveCumulativeBases(m *Model, owner map[string]*dbtSemanticModel, defs map[string]dbtMeasure) error {
	// measure name → the simple metric that publishes it.
	published := map[string]string{}
	for i := range m.Metrics {
		mt := &m.Metrics[i]
		if mt.IsDerived() || mt.IsWindow() {
			continue
		}
		for name, ms := range defs {
			if owner[name] != nil && owner[name].Name == mt.Entity && ms.column() == mt.Expr &&
				strings.EqualFold(ms.Agg, mt.Agg) {
				if _, taken := published[name]; !taken {
					published[name] = mt.Name
				}
			}
		}
	}

	var synthesized []Metric
	for i := range m.Metrics {
		mt := &m.Metrics[i]
		if !mt.IsWindow() || mt.Of == "" {
			continue
		}
		if m.Metric(mt.Of) != nil {
			continue // already a metric name
		}
		if name, ok := published[mt.Of]; ok {
			mt.Of = name
			continue
		}
		measure := mt.Of
		sm, ok := owner[measure]
		if !ok {
			return fmt.Errorf("metric %q accumulates measure %q, which no semantic model declares", mt.Name, measure)
		}
		base, err := bindMeasure(Metric{
			Name:        measure,
			Description: firstNonEmpty(defs[measure].Description, "Base measure behind "+mt.Name+"."),
		}, measure, owner, defs)
		if err != nil {
			return fmt.Errorf("metric %q: %w", mt.Name, err)
		}
		base.Entity = sm.Name
		synthesized = append(synthesized, base)
	}
	m.Metrics = append(m.Metrics, synthesized...)
	return nil
}

func primaryColumn(sm *dbtSemanticModel, entity string) string {
	if sm == nil {
		return entity
	}
	for _, e := range sm.Entities {
		if e.Name == entity {
			return e.column()
		}
	}
	return entity
}

func convertDBTMetric(dm dbtMetric, owner map[string]*dbtSemanticModel, defs map[string]dbtMeasure) (Metric, error) {
	out := Metric{
		Name:        dm.Name,
		Description: firstNonEmpty(dm.Description, dm.Label),
		Meta:        dm.Meta,
	}
	if dm.Label != "" && !strings.EqualFold(dm.Label, dm.Name) {
		out.Synonyms = append(out.Synonyms, dm.Label)
	}

	switch strings.ToLower(dm.Type) {
	case "simple", "":
		if dm.TypeParams.Measure == nil {
			return out, fmt.Errorf("a simple metric must name a measure")
		}
		return bindMeasure(out, dm.TypeParams.Measure.Name, owner, defs)

	case "ratio":
		if dm.TypeParams.Numerator == nil || dm.TypeParams.Denominator == nil {
			return out, fmt.Errorf("a ratio metric needs both a numerator and a denominator")
		}
		// nullif guards the zero denominator that a ratio over a filtered slice
		// reaches sooner or later.
		out.Formula = fmt.Sprintf("%s / nullif(%s, 0)",
			dm.TypeParams.Numerator.Name, dm.TypeParams.Denominator.Name)
		out.Additivity = NonAdditive
		return out, nil

	case "derived":
		if dm.TypeParams.Expr == "" {
			return out, fmt.Errorf("a derived metric needs an expr")
		}
		// dbt lets an input be aliased inside the expr. Rewrite each alias back
		// to the metric it stands for, since this layer resolves by name.
		expr := dm.TypeParams.Expr
		for _, in := range dm.TypeParams.Metrics {
			if in.Alias != "" && in.Alias != in.Name {
				expr = renameIdent(expr, in.Alias, in.Name)
			}
		}
		out.Formula = expr
		return out, nil

	case "cumulative":
		measure, window, grainToDate := "", dm.TypeParams.Window, dm.TypeParams.GrainToDate
		if dm.TypeParams.Measure != nil {
			measure = dm.TypeParams.Measure.Name
		}
		if c := dm.TypeParams.Cumulative; c != nil {
			window = firstNonEmpty(c.Window, window)
			grainToDate = firstNonEmpty(c.GrainToDate, grainToDate)
		}
		if measure == "" {
			return out, fmt.Errorf("a cumulative metric must name a measure")
		}
		// The measure becomes a hidden base metric name; the cumulative wraps it.
		out.Of = measure
		w, err := cumulativeWindow(window)
		if err != nil {
			return out, err
		}
		out.Window = w
		if grainToDate != "" {
			if !validPeriod(strings.ToLower(grainToDate)) {
				return out, fmt.Errorf("grain_to_date %q is not a period this layer truncates to (day|week|month|quarter|year)", grainToDate)
			}
			out.Reset = strings.ToLower(grainToDate)
		}
		return out, nil

	default:
		return out, fmt.Errorf("metric type %q is not supported: this layer compiles simple, ratio, derived and "+
			"cumulative metrics, and would have to approximate the rest", dm.Type)
	}
}

// bindMeasure attaches a simple metric to the measure — and therefore the
// dataset and grain — it aggregates.
func bindMeasure(out Metric, name string, owner map[string]*dbtSemanticModel, defs map[string]dbtMeasure) (Metric, error) {
	sm, ok := owner[name]
	if !ok {
		return out, fmt.Errorf("names measure %q, which no semantic model declares", name)
	}
	ms := defs[name]
	agg := strings.ToLower(ms.Agg)
	switch agg {
	case "sum", "count", "avg", "min", "max":
	case "count_distinct":
	case "average":
		agg = "avg"
	case "sum_boolean":
		// SUM over a boolean is a count of trues; spelling it as a sum keeps the
		// additivity reasoning right.
		agg = "sum"
	case "":
		return out, fmt.Errorf("measure %q declares no aggregation", name)
	case "median", "percentile":
		return out, fmt.Errorf("measure %q aggregates with %q, which this layer cannot roll up safely across a join: "+
			"a median of medians is not a median", name, ms.Agg)
	default:
		return out, fmt.Errorf("measure %q aggregates with %q, which this layer does not compile", name, ms.Agg)
	}
	out.Entity, out.Agg, out.Expr = sm.Name, agg, ms.column()
	if out.Description == "" {
		out.Description = ms.Description
	}
	return out, nil
}

// cumulativeWindow maps dbt's "N period" window onto this layer's spelling.
// An unbounded window (no window, no grain_to_date) is a running total.
func cumulativeWindow(window string) (string, error) {
	w := strings.TrimSpace(strings.ToLower(window))
	if w == "" {
		return "cumulative", nil
	}
	fields := strings.Fields(w)
	if len(fields) != 2 {
		return "", fmt.Errorf("window %q is not \"N period\" (e.g. \"7 days\")", window)
	}
	var n int
	if _, err := fmt.Sscanf(fields[0], "%d", &n); err != nil || n < 1 {
		return "", fmt.Errorf("window %q does not start with a positive count", window)
	}
	// dbt's window counts periods of the metric's own grain, and so does
	// rolling:N — the rows this layer produces are already one per period.
	return fmt.Sprintf("rolling:%d", n), nil
}

// renameIdent rewrites whole-word occurrences of from → to in a SQL fragment,
// leaving quoted regions and qualified references alone.
func renameIdent(expr, from, to string) string {
	return scanSQL(expr, nil, func(t token) string {
		if t.bare() && t.word == from {
			return to
		}
		return t.word
	})
}
