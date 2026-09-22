package semantic

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Semantic-interchange import
//
// The interchange document (Ossie/OSI-shaped YAML, or the osi_document.json a
// dbt parse emits) is the BOUNDARY format, not this compiler's internal one.
// That split is deliberate. An interchange spec still at 0.x, with fields like
// default_aggregation still in proposal, is a fine contract to import from and
// a poor one to build a compiler's guarantees on: every round trip through a
// converter is a chance to lose the one field the refusals depend on.
//
// So: import once, validate hard at the door, and compile against the model
// this package controls.
//
// Two mismatches are real and are handled explicitly rather than papered over:
//
//  1. An interchange metric carries a whole SQL expression per dialect
//     ("SUM(shifts.hours_worked)"), while this layer stores the aggregation and
//     the row-level expression separately — because it needs to KNOW the
//     aggregation to reason about additivity and window safety. The ANSI
//     expression is parsed into those two parts; an expression too complex to
//     split is refused with the name of the metric, not silently passed through
//     as an opaque blob that the safety checks could no longer see into.
//
//  2. Relationships are foreign keys and carry no cardinality, while every
//     refusal in this package is grounded in cardinality. It is INFERRED only
//     where the keys make it certain (a foreign key landing exactly on the
//     target's declared primary key is many-to-one) and otherwise REFUSED. A
//     guessed cardinality is precisely the silent fan-out this layer exists to
//     prevent, and guessing it at import time would be the worst possible place
//     to introduce one.
// ---------------------------------------------------------------------------

// --- wire shapes -----------------------------------------------------------

type ossieDoc struct {
	SemanticModel ossieModelList `yaml:"semantic_model" json:"semantic_model"`
}

// ossieModelList accepts one domain or a list of them.
type ossieModelList []ossieModel

func (l *ossieModelList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.SequenceNode:
		var many []ossieModel
		if err := n.Decode(&many); err != nil {
			return err
		}
		*l = many
	case yaml.MappingNode:
		var one ossieModel
		if err := n.Decode(&one); err != nil {
			return err
		}
		*l = []ossieModel{one}
	default:
		return fmt.Errorf("semantic_model must be a mapping or a list of mappings")
	}
	return nil
}

type ossieModel struct {
	Name          string              `yaml:"name"`
	Description   string              `yaml:"description"`
	AIContext     ossieAIContext      `yaml:"ai_context"`
	Datasets      []ossieDataset      `yaml:"datasets"`
	Metrics       []ossieMetric       `yaml:"metrics"`
	Dimensions    []ossieDimension    `yaml:"dimensions"`
	Relationships []ossieRelationship `yaml:"relationships"`
}

type ossieAIContext struct {
	Instructions string `yaml:"instructions"`
}

type ossieDataset struct {
	Name        string           `yaml:"name"`
	Source      string           `yaml:"source"`
	Table       string           `yaml:"table"` // alternate spelling
	Description string           `yaml:"description"`
	PrimaryKey  StringList       `yaml:"primary_key"`
	Grain       string           `yaml:"grain"`
	GrainStatus string           `yaml:"grain_status"`
	Fields      []ossieField     `yaml:"fields"`
	Metrics     []ossieMetric    `yaml:"metrics"`
	Dimensions  []ossieDimension `yaml:"dimensions"`
}

func (d ossieDataset) table() string {
	if d.Source != "" {
		return d.Source
	}
	return d.Table
}

// ossieField is a row-level attribute. Whether it is exposed as a dimension is
// decided by the PRESENCE of a `dimension:` key, not by its contents — the spec
// writes `dimension: {}` to mean "yes, with defaults", and a nil-vs-empty map
// distinction is too fragile to hang that on, so presence is captured directly.
type ossieField struct {
	Name        string
	Description string
	Synonyms    []string
	IsDimension bool
	IsTime      bool
	Column      string
}

func (f *ossieField) UnmarshalYAML(n *yaml.Node) error {
	var raw struct {
		Name        string    `yaml:"name"`
		Column      string    `yaml:"column"`
		Description string    `yaml:"description"`
		Synonyms    []string  `yaml:"synonyms"`
		Dimension   yaml.Node `yaml:"dimension"`
	}
	if err := n.Decode(&raw); err != nil {
		return err
	}
	f.Name, f.Column, f.Description, f.Synonyms = raw.Name, raw.Column, raw.Description, raw.Synonyms
	// A zero Kind means the key was absent. It must be a value field, not a
	// pointer: yaml.v3 allocates a *yaml.Node for a nested key and leaves it
	// empty, so a pointer says nothing about whether the key was there —
	// which would have silently made every field a dimension, or none.
	if raw.Dimension.Kind == 0 {
		return nil
	}
	f.IsDimension = true
	var spec struct {
		IsTime      bool     `yaml:"is_time"`
		Type        string   `yaml:"type"`
		Synonyms    []string `yaml:"synonyms"`
		Description string   `yaml:"description"`
	}
	// `dimension:` with a null value is still a declaration, just an empty one.
	if raw.Dimension.Kind == yaml.MappingNode {
		if err := raw.Dimension.Decode(&spec); err != nil {
			return fmt.Errorf("field %q: %w", raw.Name, err)
		}
	}
	f.IsTime = spec.IsTime || strings.EqualFold(spec.Type, "time")
	if len(spec.Synonyms) > 0 {
		f.Synonyms = append(f.Synonyms, spec.Synonyms...)
	}
	if f.Description == "" {
		f.Description = spec.Description
	}
	return nil
}

type ossieDimension struct {
	Name        string   `yaml:"name"`
	Dataset     string   `yaml:"dataset"`
	Field       string   `yaml:"field"`
	Column      string   `yaml:"column"`
	Description string   `yaml:"description"`
	Synonyms    []string `yaml:"synonyms"`
	IsTime      bool     `yaml:"is_time"`
	Type        string   `yaml:"type"`
}

type ossieMetric struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	Synonyms    []string          `yaml:"synonyms"`
	Dataset     string            `yaml:"dataset"`
	Expression  ossieExpression   `yaml:"expression"`
	Extensions  ossieExtensions   `yaml:"custom_extensions"`
	Meta        map[string]string `yaml:"meta"`
}

type ossieExpression struct {
	Dialects []ossieDialectExpr `yaml:"dialects"`
}

type ossieDialectExpr struct {
	Dialect    string `yaml:"dialect"`
	Expression string `yaml:"expression"`
}

// ansi returns the baseline expression: the ANSI_SQL dialect entry, or the sole
// entry when only one is given. Engine-specific overrides are deliberately not
// used for the import — this layer re-emits per-dialect SQL itself, and an
// override written for one warehouse would be wrong on every other.
func (e ossieExpression) ansi() (string, error) {
	if len(e.Dialects) == 0 {
		return "", fmt.Errorf("expression has no dialects")
	}
	for _, d := range e.Dialects {
		if strings.EqualFold(d.Dialect, "ANSI_SQL") || strings.EqualFold(d.Dialect, "ansi") {
			return d.Expression, nil
		}
	}
	if len(e.Dialects) == 1 {
		return e.Dialects[0].Expression, nil
	}
	var names []string
	for _, d := range e.Dialects {
		names = append(names, d.Dialect)
	}
	return "", fmt.Errorf("no ANSI_SQL dialect (found %s): this layer compiles its own per-engine SQL and needs the portable form",
		strings.Join(names, ", "))
}

// ossieExtensions is the documented escape hatch: when an expression cannot be
// split automatically, the author states the parts directly rather than losing
// the metric.
type ossieExtensions struct {
	SemanticGo *ossieOverride `yaml:"semantic_go"`
}

type ossieOverride struct {
	Entity     string            `yaml:"entity"`
	Agg        string            `yaml:"agg"`
	Expr       string            `yaml:"expr"`
	Formula    string            `yaml:"formula"`
	Of         string            `yaml:"of"`
	Window     string            `yaml:"window"`
	Reset      string            `yaml:"reset"`
	Additivity string            `yaml:"additivity"`
	Roles      []string          `yaml:"roles"`
	Meta       map[string]string `yaml:"meta"`
}

// ossieRelationship accepts the nested form (from/to objects) and the flat one.
type ossieRelationship struct {
	Name string        `yaml:"name"`
	From ossieEndpoint `yaml:"from"`
	To   ossieEndpoint `yaml:"to"`

	FromDataset string     `yaml:"from_dataset"`
	FromFields  StringList `yaml:"from_fields"`
	ToDataset   string     `yaml:"to_dataset"`
	ToFields    StringList `yaml:"to_fields"`

	Type        string `yaml:"type"`
	Cardinality string `yaml:"cardinality"`
}

// ossieEndpoint is one side of a relationship. It accepts a bare dataset name
// as a scalar, or {dataset, fields}.
type ossieEndpoint struct {
	Dataset string
	Fields  StringList
}

func (e *ossieEndpoint) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&e.Dataset)
	}
	var raw struct {
		Dataset string     `yaml:"dataset"`
		Entity  string     `yaml:"entity"`
		Fields  StringList `yaml:"fields"`
		Field   StringList `yaml:"field"`
		Columns StringList `yaml:"columns"`
	}
	if err := n.Decode(&raw); err != nil {
		return err
	}
	e.Dataset = raw.Dataset
	if e.Dataset == "" {
		e.Dataset = raw.Entity
	}
	switch {
	case len(raw.Fields) > 0:
		e.Fields = raw.Fields
	case len(raw.Field) > 0:
		e.Fields = raw.Field
	default:
		e.Fields = raw.Columns
	}
	return nil
}

func (r ossieRelationship) endpoints() (fromDS string, fromF []string, toDS string, toF []string) {
	fromDS, fromF = r.From.Dataset, r.From.Fields
	if fromDS == "" {
		fromDS, fromF = r.FromDataset, r.FromFields
	}
	toDS, toF = r.To.Dataset, r.To.Fields
	if toDS == "" {
		toDS, toF = r.ToDataset, r.ToFields
	}
	return
}

func (r ossieRelationship) label() string {
	if r.Name != "" {
		return r.Name
	}
	f, _, t, _ := r.endpoints()
	return f + "→" + t
}

// --- import ----------------------------------------------------------------

// ImportOssie parses an interchange document (YAML, or the JSON a dbt parse
// writes — JSON is valid YAML) and returns one domain per semantic_model entry.
func ImportOssie(data []byte) ([]Domain, error) {
	var doc ossieDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse interchange document: %w", err)
	}
	if len(doc.SemanticModel) == 0 {
		// Tolerate a bare domain document with no semantic_model wrapper.
		var bare ossieModel
		if err := yaml.Unmarshal(data, &bare); err == nil && len(bare.Datasets) > 0 {
			doc.SemanticModel = ossieModelList{bare}
		} else {
			return nil, fmt.Errorf("document declares no semantic_model")
		}
	}
	out := make([]Domain, 0, len(doc.SemanticModel))
	for _, om := range doc.SemanticModel {
		m, err := convertOssieModel(om)
		if err != nil {
			return nil, fmt.Errorf("semantic_model %q: %w", om.Name, err)
		}
		out = append(out, Domain{
			Name:         om.Name,
			Description:  om.Description,
			Instructions: om.AIContext.Instructions,
			Model:        m,
		})
	}
	return out, nil
}

// ImportOssieFile reads and imports an interchange document from disk.
func ImportOssieFile(path string) ([]Domain, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ImportOssie(data)
}

// OssieDomainNamed picks one domain out of an imported document by name.
//
// Deprecated: build a Catalog and use Get, which also handles a directory of
// models and reports the published names on a miss.
func OssieDomainNamed(domains []Domain, name string) (*Model, error) {
	c, err := NewCatalog(domains...)
	if err != nil {
		return nil, err
	}
	d, err := c.Get(name)
	if err != nil {
		return nil, err
	}
	return d.Model, nil
}

func convertOssieModel(om ossieModel) (*Model, error) {
	m := &Model{}
	known := map[string]*ossieDataset{}

	for i := range om.Datasets {
		ds := &om.Datasets[i]
		if ds.Name == "" {
			return nil, fmt.Errorf("a dataset has no name")
		}
		if ds.table() == "" {
			return nil, fmt.Errorf("dataset %q has no source/table", ds.Name)
		}
		// Rule 1 of the grain contract: no dataset enters the layer without a
		// declared key tuple. There is no default and no inference — a dataset
		// whose grain nobody wrote down is a dataset nobody has checked.
		if len(ds.PrimaryKey) == 0 {
			return nil, fmt.Errorf("dataset %q declares no primary_key: every dataset must state its grain as a key tuple "+
				"before it can be aggregated over", ds.Name)
		}
		known[ds.Name] = ds
		m.Entities = append(m.Entities, Entity{
			Name:        ds.Name,
			Table:       ds.table(),
			PrimaryKey:  ds.PrimaryKey,
			Grain:       ds.Grain,
			GrainStatus: ds.GrainStatus,
		})
		for _, f := range ds.Fields {
			if !f.IsDimension {
				continue
			}
			col := f.Column
			if col == "" {
				col = f.Name
			}
			m.Dimensions = append(m.Dimensions, Dimension{
				Name:     f.Name,
				Entity:   ds.Name,
				Column:   col,
				Type:     dimType(f.IsTime),
				Synonyms: f.Synonyms,
			})
		}
	}

	// Top-level and per-dataset dimension declarations.
	for _, od := range append(append([]ossieDimension{}, om.Dimensions...), collectDatasetDims(om)...) {
		if od.Dataset == "" {
			return nil, fmt.Errorf("dimension %q names no dataset", od.Name)
		}
		col := od.Column
		if col == "" {
			col = od.Field
		}
		if col == "" {
			col = od.Name
		}
		m.Dimensions = append(m.Dimensions, Dimension{
			Name:     od.Name,
			Entity:   od.Dataset,
			Column:   col,
			Type:     dimType(od.IsTime || strings.EqualFold(od.Type, "time")),
			Synonyms: od.Synonyms,
		})
	}

	// Metrics: top-level, then any declared inside a dataset (which supplies a
	// default dataset for them).
	type pending struct {
		om      ossieMetric
		dataset string
	}
	var metrics []pending
	for _, mt := range om.Metrics {
		metrics = append(metrics, pending{mt, mt.Dataset})
	}
	for i := range om.Datasets {
		for _, mt := range om.Datasets[i].Metrics {
			ds := mt.Dataset
			if ds == "" {
				ds = om.Datasets[i].Name
			}
			metrics = append(metrics, pending{mt, ds})
		}
	}
	for _, p := range metrics {
		converted, err := convertOssieMetric(p.om, p.dataset, known)
		if err != nil {
			return nil, fmt.Errorf("metric %q: %w", p.om.Name, err)
		}
		m.Metrics = append(m.Metrics, converted)
	}

	for _, r := range om.Relationships {
		j, err := convertOssieRelationship(r, known)
		if err != nil {
			return nil, fmt.Errorf("relationship %q: %w", r.label(), err)
		}
		m.Joins = append(m.Joins, j)
	}

	if err := m.Index(); err != nil {
		return nil, err
	}
	return m, nil
}

func collectDatasetDims(om ossieModel) []ossieDimension {
	var out []ossieDimension
	for i := range om.Datasets {
		for _, d := range om.Datasets[i].Dimensions {
			if d.Dataset == "" {
				d.Dataset = om.Datasets[i].Name
			}
			out = append(out, d)
		}
	}
	return out
}

func dimType(isTime bool) string {
	if isTime {
		return "time"
	}
	return "categorical"
}

func convertOssieMetric(om ossieMetric, defaultDataset string, known map[string]*ossieDataset) (Metric, error) {
	out := Metric{
		Name:        om.Name,
		Description: om.Description,
		Synonyms:    om.Synonyms,
		Meta:        om.Meta,
	}
	if out.Name == "" {
		return out, fmt.Errorf("metric has no name")
	}

	// The escape hatch wins outright: an author who has stated the parts has
	// already answered the question the parser would be guessing at.
	if ov := om.Extensions.SemanticGo; ov != nil {
		out.Entity, out.Agg, out.Expr = ov.Entity, ov.Agg, ov.Expr
		out.Formula, out.Of, out.Window, out.Reset = ov.Formula, ov.Of, ov.Window, ov.Reset
		out.Additivity, out.Roles = ov.Additivity, ov.Roles
		if ov.Meta != nil {
			out.Meta = ov.Meta
		}
		if out.Entity == "" {
			out.Entity = defaultDataset
		}
		if out.Formula == "" && out.Window == "" && out.Agg == "" {
			return out, fmt.Errorf("custom_extensions.semantic_go sets neither agg, formula nor window")
		}
		return out, nil
	}

	expr, err := om.Expression.ansi()
	if err != nil {
		return out, err
	}
	agg, inner, err := splitAggregation(expr)
	if err != nil {
		return out, fmt.Errorf("%w\n  expression: %s\n  fix: state the parts under custom_extensions.semantic_go "+
			"(entity/agg/expr), or express it as a derived metric over metrics that are themselves aggregations", err, expr)
	}
	entity, stripped, err := datasetOf(inner, known)
	if err != nil {
		return out, err
	}
	if entity == "" {
		entity = defaultDataset
	}
	if entity == "" {
		return out, fmt.Errorf("no dataset: the expression qualifies no column with a known dataset and the metric declares none")
	}
	if known[entity] == nil {
		return out, fmt.Errorf("names dataset %q, which this document does not declare", entity)
	}
	out.Entity, out.Agg, out.Expr = entity, agg, stripped
	return out, nil
}

// aggCallRe matches a single outermost aggregate call.
var aggCallRe = regexp.MustCompile(`(?is)^\s*([A-Za-z_][A-Za-z0-9_]*)\s*\((.*)\)\s*$`)

// splitAggregation pulls "SUM(x)" apart into ("sum", "x").
//
// The split is required rather than optional: this layer refuses a cumulative
// sum of a distinct count, and picks the additivity class of a derived formula
// from its parts. Both of those need to know WHICH aggregation a metric is. A
// metric imported as an opaque SQL string would still compile — and would
// quietly leave those guards with nothing to check.
func splitAggregation(expr string) (agg string, inner string, err error) {
	mt := aggCallRe.FindStringSubmatch(expr)
	if mt == nil {
		return "", "", fmt.Errorf("expression is not a single aggregate call")
	}
	fn, body := strings.ToLower(mt[1]), mt[2]
	// Guard against "SUM(a) - SUM(b)", which matches the regex greedily but is
	// two aggregations, not one.
	if !balancedWhole(body) {
		return "", "", fmt.Errorf("expression is not a single aggregate call (it closes and reopens parentheses)")
	}
	body = strings.TrimSpace(body)
	switch fn {
	case "sum", "avg", "min", "max":
		return fn, body, nil
	case "count":
		if rest := strings.TrimSpace(body); len(rest) > 9 && strings.EqualFold(rest[:9], "distinct ") {
			return "count_distinct", strings.TrimSpace(rest[9:]), nil
		}
		return "count", body, nil
	case "count_distinct":
		return "count_distinct", body, nil
	default:
		return "", "", fmt.Errorf("aggregation %q is not one of sum|count|count_distinct|avg|min|max", fn)
	}
}

// balancedWhole reports whether the body of a call never closes past depth 0,
// which is what distinguishes SUM(a * (b + c)) from SUM(a) - SUM(b).
func balancedWhole(body string) bool {
	depth := 0
	for _, r := range body {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

var qualifiedRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*\.\s*([A-Za-z_][A-Za-z0-9_]*)`)

// datasetOf reads the dataset a row-level expression belongs to off its column
// qualifiers, and refuses an expression that spans two.
//
// Spanning datasets is not a parser limitation. A base metric is aggregated at
// exactly one dataset's grain — that is what makes aggregate-then-join safe —
// so "SUM(shifts.hours * rates.multiplier)" has no single grain to aggregate
// at, and the honest answer is to model it as two metrics and a formula.
func datasetOf(expr string, known map[string]*ossieDataset) (string, string, error) {
	var found []string
	seen := map[string]bool{}
	for _, mt := range qualifiedRe.FindAllStringSubmatch(expr, -1) {
		ns := mt[1]
		if known[ns] == nil || seen[ns] {
			continue
		}
		seen[ns] = true
		found = append(found, ns)
	}
	switch len(found) {
	case 0:
		return "", expr, nil
	case 1:
		return found[0], expr, nil
	default:
		return "", "", fmt.Errorf("expression spans datasets %s: a base metric aggregates over exactly one dataset at its own grain — "+
			"define one metric per dataset and combine them with a formula", strings.Join(found, " and "))
	}
}

// convertOssieRelationship turns a foreign key into a declared, directed edge.
func convertOssieRelationship(r ossieRelationship, known map[string]*ossieDataset) (Join, error) {
	fromDS, fromF, toDS, toF := r.endpoints()
	if fromDS == "" || toDS == "" {
		return Join{}, fmt.Errorf("needs both a from and a to dataset")
	}
	if known[fromDS] == nil {
		return Join{}, fmt.Errorf("names dataset %q, which this document does not declare", fromDS)
	}
	if known[toDS] == nil {
		return Join{}, fmt.Errorf("names dataset %q, which this document does not declare", toDS)
	}
	if len(fromF) == 0 || len(toF) == 0 {
		return Join{}, fmt.Errorf("needs key columns on both sides")
	}
	if len(fromF) != len(toF) {
		return Join{}, fmt.Errorf("has %d column(s) on the from side and %d on the to side", len(fromF), len(toF))
	}
	card, err := cardinalityOf(r, fromDS, fromF, toDS, toF, known)
	if err != nil {
		return Join{}, err
	}
	return Join{From: fromDS, To: toDS, FromKey: fromF, ToKey: toF, Cardinality: card}, nil
}

// cardinalityOf takes a declared cardinality when there is one, and otherwise
// infers it ONLY from a key landing exactly on a declared primary key — the one
// case where the answer is a fact about the model rather than a guess.
func cardinalityOf(r ossieRelationship, fromDS string, fromF []string, toDS string, toF []string, known map[string]*ossieDataset) (string, error) {
	if c := normalizeCardinality(firstNonEmpty(r.Type, r.Cardinality)); c != "" {
		return c, nil
	}
	toIsPK := sameColumns(toF, known[toDS].PrimaryKey)
	fromIsPK := sameColumns(fromF, known[fromDS].PrimaryKey)
	switch {
	case toIsPK && !fromIsPK:
		return "many_to_one", nil
	case fromIsPK && !toIsPK:
		return "one_to_many", nil
	case toIsPK && fromIsPK:
		// Both sides unique: one-to-one, which is many-to-one in both
		// directions and safe to traverse either way. Declaring it as
		// many_to_one is exact, not a rounding.
		return "many_to_one", nil
	}
	return "", fmt.Errorf("cardinality cannot be inferred: %s(%s) → %s(%s) lands on neither dataset's declared primary key "+
		"(%s has %s, %s has %s). Declare `type: many_to_one|one_to_many|many_to_many` — "+
		"an inferred cardinality is exactly the silent fan-out this layer exists to prevent",
		fromDS, strings.Join(fromF, ", "), toDS, strings.Join(toF, ", "),
		fromDS, known[fromDS].PrimaryKey, toDS, known[toDS].PrimaryKey)
}

func normalizeCardinality(s string) string {
	switch strings.ToLower(strings.NewReplacer("-", "_", " ", "_").Replace(strings.TrimSpace(s))) {
	case "many_to_one", "n:1", "n_1", "manytoone":
		return "many_to_one"
	case "one_to_many", "1:n", "1_n", "onetomany":
		return "one_to_many"
	case "many_to_many", "n:m", "n_m", "manytomany":
		return "many_to_many"
	case "one_to_one", "1:1", "1_1", "onetoone":
		return "many_to_one"
	default:
		return ""
	}
}

func sameColumns(a, b []string) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	used := make([]bool, len(b))
	for _, x := range a {
		hit := false
		for i, y := range b {
			if !used[i] && strings.EqualFold(x, y) {
				used[i], hit = true, true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
