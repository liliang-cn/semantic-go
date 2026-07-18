package semantic

import (
	"fmt"
	"sort"
	"strings"
)

// normName normalizes a metric name or synonym for case-insensitive matching:
// lowercased and underscores/spaces folded, so "Store Region" == "store_region".
func normName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.ReplaceAll(s, "_", " ")
}

// ResolveMetricName maps a metric name or a declared synonym to its canonical
// metric name. An exact canonical name always resolves to itself unchanged;
// otherwise matching is case-insensitive over names and synonyms. ok is false
// when nothing matches.
func (m *Model) ResolveMetricName(name string) (string, bool) {
	if m.metric != nil {
		if _, ok := m.metric[name]; ok {
			return name, true // exact canonical — unchanged
		}
	}
	if m.metricSyn != nil {
		if c, ok := m.metricSyn[normName(name)]; ok {
			return c, true
		}
	}
	return "", false
}

// ResolveMetrics rewrites q.Metrics from synonyms to canonical names in place
// (canonical names pass through unchanged). An unknown name returns an error
// naming the closest known metrics. Dimensions are untouched — they carry no
// synonyms. A fresh slice is allocated, so the caller's original is not mutated.
func (m *Model) ResolveMetrics(q *Query) error {
	if len(q.Metrics) == 0 {
		return nil
	}
	out := make([]string, len(q.Metrics))
	for i, name := range q.Metrics {
		canon, ok := m.ResolveMetricName(name)
		if !ok {
			if sugg := m.SuggestMetricNames(name, 3); len(sugg) > 0 {
				return fmt.Errorf("unknown metric %q; did you mean %s?", name, humanList(sugg))
			}
			return fmt.Errorf("unknown metric %q (known: %s)", name, strings.Join(m.MetricNames(), ", "))
		}
		out[i] = canon
	}
	q.Metrics = out
	return nil
}

// ResolveDimensionName maps a dimension name or a declared synonym to its
// canonical dimension name. An exact canonical name resolves to itself; ok is
// false when nothing matches.
func (m *Model) ResolveDimensionName(name string) (string, bool) {
	if m.dimension != nil {
		if _, ok := m.dimension[name]; ok {
			return name, true
		}
	}
	if m.dimSyn != nil {
		if c, ok := m.dimSyn[normName(name)]; ok {
			return c, true
		}
	}
	return "", false
}

// ResolveGroupBy rewrites q.GroupBy from dimension synonyms to canonical names
// in place (canonical names pass through unchanged). Unknown names are left as
// is so the compiler reports them with its own dimension diagnostics. A fresh
// slice is allocated, so the caller's original is not mutated.
func (m *Model) ResolveGroupBy(q *Query) {
	if len(q.GroupBy) == 0 {
		return
	}
	out := make([]string, len(q.GroupBy))
	for i, name := range q.GroupBy {
		if canon, ok := m.ResolveDimensionName(name); ok {
			out[i] = canon
		} else {
			out[i] = name
		}
	}
	q.GroupBy = out
}

// SuggestMetricNames returns up to n canonical metric names whose name or
// synonyms are closest to the given (unknown) name — for a helpful error.
// Substring matches rank first, then smallest edit distance.
func (m *Model) SuggestMetricNames(name string, n int) []string {
	want := normName(name)
	type scored struct {
		metric string
		sub    bool
		dist   int
	}
	best := map[string]scored{}
	consider := func(metric, candidate string) {
		c := normName(candidate)
		if c == "" {
			return
		}
		sub := strings.Contains(c, want) || strings.Contains(want, c)
		d := levenshtein(want, c)
		if cur, ok := best[metric]; !ok || better(sub, d, cur.sub, cur.dist) {
			best[metric] = scored{metric: metric, sub: sub, dist: d}
		}
	}
	for i := range m.Metrics {
		consider(m.Metrics[i].Name, m.Metrics[i].Name)
		for _, syn := range m.Metrics[i].Synonyms {
			consider(m.Metrics[i].Name, syn)
		}
	}
	all := make([]scored, 0, len(best))
	for _, s := range best {
		all = append(all, s)
	}
	sort.SliceStable(all, func(i, j int) bool {
		return better(all[i].sub, all[i].dist, all[j].sub, all[j].dist)
	})
	out := make([]string, 0, n)
	for _, s := range all {
		// Skip candidates that are neither a substring match nor reasonably close.
		if !s.sub && s.dist > len(want)+2 {
			continue
		}
		out = append(out, s.metric)
		if len(out) >= n {
			break
		}
	}
	return out
}

// better reports whether (subA,distA) is a better suggestion than (subB,distB):
// a substring match wins; otherwise the smaller edit distance wins.
func better(subA bool, distA int, subB bool, distB int) bool {
	if subA != subB {
		return subA
	}
	return distA < distB
}

func humanList(ss []string) string {
	quoted := make([]string, len(ss))
	for i, s := range ss {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(quoted, ", ")
}

// levenshtein is the classic edit distance over runes (synonyms may be CJK).
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
