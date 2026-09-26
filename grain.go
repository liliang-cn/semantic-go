package semantic

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// StringList is a YAML scalar-or-sequence: `primary_key: order_id` and
// `primary_key: [guard_id, shift_id]` both decode to it. The scalar form is
// kept because a single-column key is the common case and quoting it as a
// one-element list everywhere is noise.
type StringList []string

// UnmarshalYAML accepts a scalar or a sequence of scalars.
func (s *StringList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var one string
		if err := n.Decode(&one); err != nil {
			return err
		}
		*s = StringList{one}
		return nil
	case yaml.SequenceNode:
		var many []string
		if err := n.Decode(&many); err != nil {
			return err
		}
		*s = many
		return nil
	default:
		return fmt.Errorf("expected a string or a list of strings, got %v", n.Kind)
	}
}

// MarshalYAML writes a one-element list back as a scalar, so a round trip
// through the loader doesn't rewrite every model file.
func (s StringList) MarshalYAML() (any, error) {
	if len(s) == 1 {
		return s[0], nil
	}
	return []string(s), nil
}

func (s StringList) String() string { return strings.Join(s, ", ") }

// GrainInfo is what describe_grain returns: a dataset's declared grain and
// whether the build-time uniqueness test has vouched for it.
type GrainInfo struct {
	Entity string   `json:"entity"`
	Table  string   `json:"table"`
	Grain  []string `json:"grain"`            // the declared key tuple
	Note   string   `json:"note,omitempty"`   // the modeller's sentence, if any
	Status string   `json:"status"`           // pass | fail | untested
	Usable bool     `json:"usable"`           // false ⇒ the compiler refuses this entity
	Reason string   `json:"reason,omitempty"` // why not usable
}

// Untested is the reported status of an entity whose grain test has not run.
const Untested = "untested"

// DescribeGrain returns the declared grain and validation status of one entity.
// This is the lookup half of the grain contract; the compiler enforces the same
// rule independently (see Compile), so a caller that skips this call cannot
// thereby reach an unvalidated dataset.
func (m *Model) DescribeGrain(entity string) (GrainInfo, error) {
	e := m.Entity(entity)
	if e == nil {
		return GrainInfo{}, fmt.Errorf("unknown entity %q (known: %s)", entity, strings.Join(m.EntityNames(), ", "))
	}
	gi := GrainInfo{
		Entity: e.Name,
		Table:  e.Table,
		Grain:  e.PrimaryKey,
		Note:   e.Grain,
		Status: e.GrainStatus,
		Usable: true,
	}
	if gi.Status == "" {
		gi.Status = Untested
	}
	if e.GrainStatus == GrainFail {
		gi.Usable = false
		gi.Reason = fmt.Sprintf("grain test failed: (%s) is not unique in %s — the dataset is withheld until it is fixed",
			e.PrimaryKey, e.Table)
	}
	return gi, nil
}

// EntityNames returns entity names in definition order.
func (m *Model) EntityNames() []string {
	out := make([]string, len(m.Entities))
	for i := range m.Entities {
		out[i] = m.Entities[i].Name
	}
	return out
}

// GrainCheckSQL emits the uniqueness test for one entity's declared grain:
// rows returned means the grain is a lie. Clients on dbt get this from
// dbt_utils.unique_combination_of_columns; clients without dbt have no CI to
// lean on, and this is the same assertion they can run from anything that
// speaks SQL.
//
// The query returns the offending key tuples and their row counts, so a failure
// names the duplicates rather than only asserting that some exist.
func (m *Model) GrainCheckSQL(entity string, d Dialect) (string, error) {
	e := m.Entity(entity)
	if e == nil {
		return "", fmt.Errorf("unknown entity %q", entity)
	}
	cols := make([]string, len(e.PrimaryKey))
	for i, c := range e.PrimaryKey {
		cols[i] = d.QuoteIdent(c)
	}
	list := strings.Join(cols, ", ")
	return fmt.Sprintf(
		"SELECT %s, COUNT(*) AS %s\nFROM %s\nGROUP BY %s\nHAVING COUNT(*) > 1",
		list, d.QuoteIdent("n_rows"), QuoteTable(d, e.Table), list), nil
}

// GrainCheckSuite emits one uniqueness test per entity, in definition order —
// the whole build-time gate for a model, ready to run against any engine.
func (m *Model) GrainCheckSuite(d Dialect) []GrainCheck {
	out := make([]GrainCheck, 0, len(m.Entities))
	for i := range m.Entities {
		sql, err := m.GrainCheckSQL(m.Entities[i].Name, d)
		if err != nil {
			continue
		}
		out = append(out, GrainCheck{Entity: m.Entities[i].Name, Grain: m.Entities[i].PrimaryKey, SQL: sql})
	}
	return out
}

// GrainCheck is one entity's uniqueness assertion.
type GrainCheck struct {
	Entity string   `json:"entity"`
	Grain  []string `json:"grain"`
	SQL    string   `json:"sql"`
}
