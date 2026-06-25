package semantic

import "regexp"

// edge is one safe (many-to-one) traversal step in the join graph.
type edge struct {
	to       string
	leftCol  string // column on the "from" entity's table
	rightCol string // column on the "to" entity's table
}

// adjacency builds the many-to-one traversal graph from declared joins. Shared by
// the compiler (join planning) and reachability (valid-dimension discovery).
func (m *Model) adjacency() map[string][]edge {
	adj := map[string][]edge{}
	for _, j := range m.Joins {
		switch j.Cardinality {
		case "one_to_many":
			// many side = To, one side = From; safe edge To -> From
			adj[j.To] = append(adj[j.To], edge{to: j.From, leftCol: j.ToKey, rightCol: j.FromKey})
		case "many_to_one":
			// many side = From, one side = To; safe edge From -> To
			adj[j.From] = append(adj[j.From], edge{to: j.To, leftCol: j.FromKey, rightCol: j.ToKey})
		case "many_to_many":
			// not traversable directly — must go through a bridge entity
		}
	}
	return adj
}

// ReachableEntities returns the set of entities reachable from base by following
// only many-to-one edges (the joins that don't fan out the base grain).
func (m *Model) ReachableEntities(base string) map[string]bool {
	adj := m.adjacency()
	seen := map[string]bool{base: true}
	queue := []string{base}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, e := range adj[cur] {
			if !seen[e.to] {
				seen[e.to] = true
				queue = append(queue, e.to)
			}
		}
	}
	return seen
}

var metricRefRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// baseEntitiesOf collects the base-metric entities a metric depends on (recursing
// through derived formulas).
func (m *Model) baseEntitiesOf(name string, visiting map[string]bool) ([]string, error) {
	mt := m.Metric(name)
	if mt == nil {
		return nil, &UnknownMetricError{name}
	}
	if !mt.IsDerived() {
		return []string{mt.Entity}, nil
	}
	if visiting[name] {
		return nil, nil
	}
	visiting[name] = true
	var out []string
	for _, tok := range metricRefRe.FindAllString(mt.Formula, -1) {
		if m.Metric(tok) == nil {
			continue // SQL function/literal
		}
		sub, err := m.baseEntitiesOf(tok, visiting)
		if err != nil {
			return nil, err
		}
		out = append(out, sub...)
	}
	return out, nil
}

// DimensionsFor returns the dimensions that can slice the metric WITHOUT a fanout
// — i.e. dimensions whose entity is reachable from every base metric the metric
// depends on. (This is why "net_revenue by product_category" is rejected: refunds
// can't reach product.) Powers the get_dimensions tool so agents self-correct.
func (m *Model) DimensionsFor(metric string) ([]string, error) {
	bases, err := m.baseEntitiesOf(metric, map[string]bool{})
	if err != nil {
		return nil, err
	}
	if len(bases) == 0 {
		return nil, nil
	}
	var inter map[string]bool
	for _, e := range bases {
		r := m.ReachableEntities(e)
		if inter == nil {
			inter = r
			continue
		}
		for k := range inter {
			if !r[k] {
				delete(inter, k)
			}
		}
	}
	var out []string
	for i := range m.Dimensions {
		if inter[m.Dimensions[i].Entity] {
			out = append(out, m.Dimensions[i].Name)
		}
	}
	return out, nil
}

// UnknownMetricError is returned when a metric name doesn't exist.
type UnknownMetricError struct{ Name string }

func (e *UnknownMetricError) Error() string { return "unknown metric " + e.Name }
