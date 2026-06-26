package semantic

import "fmt"

// Issue is one finding from Lint. Error issues should fail a CI gate; Warn
// issues are advisory (the model still compiles).
type Issue struct {
	Severity string // "error" | "warn"
	Target   string // metric/dimension name
	Message  string
}

func (i Issue) String() string { return fmt.Sprintf("%-5s %s: %s", i.Severity, i.Target, i.Message) }

// Lint enforces the metadata contract a grounding agent relies on: every metric
// must describe what it means and offer at least one synonym to route to, and
// any roll-up that an agent could get wrong must be classified. Metadata is the
// agent's only map; an undescribed metric is an invitation to guess.
//
// Returns issues in model order. Callers gate on len(errors) > 0.
func Lint(m *Model) []Issue {
	var out []Issue
	for i := range m.Metrics {
		mt := &m.Metrics[i]
		if mt.Description == "" {
			out = append(out, Issue{"error", mt.Name, "missing description (the agent's only map of what this includes/excludes)"})
		}
		if len(mt.Synonyms) == 0 {
			out = append(out, Issue{"warn", mt.Name, "no synonyms — natural-language asks may not route here"})
		}
		// A metric the layer would infer as non-summable, but that was not
		// declared, is a roll-up trap waiting to happen — ask for it explicitly.
		if mt.Additivity == "" && m.Additivity(mt.Name) == NonAdditive && !mt.IsWindow() {
			out = append(out, Issue{"warn", mt.Name, "inferred non_additive (ratio/distinct) but not declared — set additivity: non_additive"})
		}
	}
	return out
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
