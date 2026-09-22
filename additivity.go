package semantic

import (
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// The semi-additive gate
//
// A semi-additive measure — an inventory level, an account balance, a headcount
// — may be summed across products, regions, anything at all EXCEPT time. Over
// time it must be picked, not added: yesterday's balance plus today's balance
// is not a balance of anything.
//
// Nothing about the wrong answer looks wrong. Sum a month of daily stock
// snapshots and you get roughly thirty times the stock, which is a large
// plausible number in a column of large plausible numbers, and it moves in the
// right direction month over month.
//
// The additivity class was already declared and already inferred; until this
// gate it was consulted only for window metrics, so the plainest possible query
// — "on-hand by month" — compiled without a murmur. That is the worst shape for
// a guardrail to have: named in the model, documented in the README, absent
// from the one path everybody takes.
//
// The rule below refuses rather than repairs. Summing over time correctly needs
// a policy nobody has stated — last of period? first? period average? — and
// each gives a different number that is defensible under different accounting.
// Picking one for the modeller would be inventing the answer.
// ---------------------------------------------------------------------------

// checkSemiAdditive refuses a query that would add a semi-additive measure
// across time. It returns nil when the query pins a point in time, which is the
// only shape in which the sum is well defined.
func (c *compiler) checkSemiAdditive(metricName string) error {
	if c.m.Additivity(metricName) != SemiAdditive {
		return nil
	}
	mt := c.m.Metric(metricName)
	if mt == nil || !addsAcrossRows(mt.Agg) {
		// MIN, MAX and COUNT DISTINCT over time are point-in-time picks or
		// set operations; only addition is the unsound one.
		return nil
	}

	var timeDims []resolvedDim
	for _, d := range c.dims {
		if d.typ == "time" {
			timeDims = append(timeDims, d)
		}
	}

	// Pinned to a single instant by a filter: every row in every group shares
	// one point in time, so the sum is across entities, not across time.
	if c.pinsAnInstant() {
		return nil
	}

	if len(timeDims) == 0 {
		return fmt.Errorf(
			"metric %q is semi_additive and this query sums it across all of time: %s is a level, not a flow, "+
				"so adding every period's value together measures nothing. "+
				"Group by a time dimension at the snapshot's own grain, or pin one date with an equality filter",
			metricName, metricName)
	}

	// A coarsened grain buckets many snapshots into one row, and the only thing
	// the compiler can do inside a bucket is add them.
	if c.q.TimeGrain != "" {
		return fmt.Errorf(
			"metric %q is semi_additive and time grain %q buckets several snapshots into one row, leaving nothing "+
				"for the compiler to do inside a bucket but add them: a level cannot be added across periods. "+
				"Drop the grain to report at the snapshot's own grain, pin one date with an equality filter, "+
				"or model the roll-up you actually want as its own metric (a period-end pick is `agg: max` over the date, "+
				"a period average is `agg: avg`) and declare its additivity",
			metricName, c.q.TimeGrain)
	}

	// Un-coarsened, and the time column is part of the measure's own declared
	// grain: each group is exactly one snapshot, so there is nothing to add
	// across. This is the payoff for making entities declare a key tuple —
	// the model already says which columns make a row unique, so "is this
	// group a single point in time" is a question it can answer.
	base := c.m.Entity(mt.Entity)
	for _, d := range timeDims {
		dim := c.m.Dimension(d.name)
		if dim == nil || dim.Entity != mt.Entity || base == nil || !inKey(base.PrimaryKey, dim.Column) {
			return fmt.Errorf(
				"metric %q is semi_additive and %q is not part of %s's declared grain (%s), "+
					"so a group may hold several snapshots and they would be added together. "+
					"Group by a time dimension that is part of the grain, or pin one date with an equality filter",
				metricName, d.name, mt.Entity, base.PrimaryKey)
		}
	}
	return nil
}

// pinsAnInstant reports whether a top-level dimension filter fixes a time
// dimension to exactly one value.
//
// Only a top-level equality counts. Inside an OR the filter admits other
// instants, and BETWEEN admits a range — both put more than one snapshot in a
// group, which is the thing being guarded against. A conservative reading here
// costs a caller one clear error message; a generous one costs them a number.
func (c *compiler) pinsAnInstant() bool {
	for _, f := range c.dimFilters {
		if f.IsGroup() {
			continue
		}
		dim := c.m.Dimension(f.Dimension)
		if dim == nil || dim.Type != "time" {
			continue
		}
		switch strings.ToLower(f.Op) {
		case "=":
			return true
		case "in":
			if len(f.Values) == 1 {
				return true
			}
		}
	}
	return false
}

// addsAcrossRows reports whether an aggregation combines rows by addition —
// the operation that is unsound over time for a level.
func addsAcrossRows(agg string) bool {
	switch strings.ToLower(agg) {
	case "sum", "count", "avg":
		return true
	}
	return false
}

func inKey(key []string, col string) bool {
	for _, k := range key {
		if strings.EqualFold(k, col) {
			return true
		}
	}
	return false
}
