package semantic

import (
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// Filter validation
//
// Everything here exists because the compiler used to answer a malformed filter
// with SILENCE. A leaf with no operator, an IN with an empty value list, a
// BETWEEN with one bound, an operator spelled `~=` — each produced no SQL and
// no error, so a query that asked to be filtered came back unfiltered, with a
// number that was larger than it should have been and entirely plausible.
//
// That is the same failure this whole package is built to prevent, sitting in
// its own filter path: a clean run, a green check, a silently wrong number. It
// was worse than a fan-out, because a fan-out at least multiplies by something
// a reader might notice.
//
// So every operator is declared with its arity, every leaf is checked before
// any SQL is built, and the renderer cannot emit an empty predicate: an operator
// it does not recognise is a programming error, not a filter that quietly
// matches everything.
// ---------------------------------------------------------------------------

// arity describes how many values an operator takes.
type arity struct {
	min, max int // max < 0 means unbounded
}

// filterOps is the whole vocabulary. Adding an operator means adding it here,
// which is what keeps the validator and the renderer from drifting apart.
var filterOps = map[string]arity{
	"=":               {1, 1},
	"!=":              {1, 1},
	">":               {1, 1},
	">=":              {1, 1},
	"<":               {1, 1},
	"<=":              {1, 1},
	"in":              {1, -1},
	"not in":          {1, -1},
	"between":         {2, 2},
	"not between":     {2, 2},
	"contains":        {1, -1},
	"not contains":    {1, -1},
	"starts with":     {1, -1},
	"not starts with": {1, -1},
	"ends with":       {1, -1},
	"not ends with":   {1, -1},
	"is null":         {0, 0},
	"is not null":     {0, 0},
}

// FilterOperators returns the supported operators, sorted — for a tool schema
// or an error message that would otherwise have to repeat the list by hand.
func FilterOperators() []string {
	out := make([]string, 0, len(filterOps))
	for op := range filterOps {
		out = append(out, op)
	}
	sortStrings(out)
	return out
}

func sortStrings(ss []string) {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j] < ss[j-1]; j-- {
			ss[j], ss[j-1] = ss[j-1], ss[j]
		}
	}
}

// validateFilter checks one filter subtree, naming the offending leaf.
func (m *Model) validateFilter(f Filter) error {
	if f.IsGroup() {
		if f.Dimension != "" || f.Metric != "" || f.Op != "" || len(f.Values) > 0 {
			return fmt.Errorf("filter sets both a member (%s%s) and an and/or group",
				f.Dimension, f.Metric)
		}
		for _, sub := range append(append([]Filter{}, f.And...), f.Or...) {
			if err := m.validateFilter(sub); err != nil {
				return err
			}
		}
		return nil
	}

	member := ""
	switch {
	case f.Dimension != "" && f.Metric != "":
		return fmt.Errorf("filter names both dimension %q and metric %q: a leaf predicates on one or the other, "+
			"because one becomes a WHERE and the other a HAVING", f.Dimension, f.Metric)
	case f.Dimension != "":
		member = f.Dimension
		if m.Dimension(member) == nil {
			return fmt.Errorf("filter names unknown dimension %q (known: %s)", member, summarizeNames(m.DimensionNames()))
		}
	case f.Metric != "":
		member = f.Metric
		if m.Metric(member) == nil {
			if sugg := m.SuggestMetricNames(member, 3); len(sugg) > 0 {
				return fmt.Errorf("filter names unknown metric %q; did you mean %s?", member, humanList(sugg))
			}
			return fmt.Errorf("filter names unknown metric %q", member)
		}
	default:
		return fmt.Errorf("filter names neither a dimension nor a metric, and has no and/or group")
	}

	op := strings.ToLower(strings.TrimSpace(f.Op))
	if op == "" {
		return fmt.Errorf("filter on %q has no operator: an operator-less filter used to compile to nothing at all, "+
			"so the query came back unfiltered. Set one of: %s", member, strings.Join(FilterOperators(), " "))
	}
	a, ok := filterOps[op]
	if !ok {
		return fmt.Errorf("filter on %q uses unknown operator %q: supported are %s",
			member, f.Op, strings.Join(FilterOperators(), " "))
	}
	if len(f.Values) < a.min || (a.max >= 0 && len(f.Values) > a.max) {
		return fmt.Errorf("filter on %q with %q got %d value(s), needs %s",
			member, op, len(f.Values), describeArity(a))
	}
	// A comparison against NULL is never true, in any engine. Written out it
	// reads like a filter and behaves like an empty result.
	for i, v := range f.Values {
		if v == nil {
			return fmt.Errorf("filter on %q has a null value at position %d: a comparison against NULL is never true — "+
				"use the `is null` operator if that is what was meant", member, i)
		}
	}
	return nil
}

func describeArity(a arity) string {
	switch {
	case a.min == a.max && a.min == 0:
		return "none"
	case a.min == a.max:
		return fmt.Sprintf("exactly %d", a.min)
	case a.max < 0:
		return fmt.Sprintf("at least %d", a.min)
	default:
		return fmt.Sprintf("between %d and %d", a.min, a.max)
	}
}

// normalizeOp lowercases and trims an operator so "IN" and " in " are one thing.
func normalizeOp(op string) string { return strings.ToLower(strings.TrimSpace(op)) }
