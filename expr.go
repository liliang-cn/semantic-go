package semantic

import "strings"

// ---------------------------------------------------------------------------
// Qualifying a metric's row-level expression
//
// A metric writes `expr: guard_id` against its own dataset and that reads
// correctly — until the compiler joins guards in to reach a dimension, and
// guards has a guard_id too. The engine then refuses the query with "ambiguous
// reference", and it refuses it at RUN time, from inside a CTE, naming a column
// the modeller never wrote a join for.
//
// So every bare column in an expression is qualified with the metric's own base
// alias before it is emitted. An already-qualified reference is left exactly as
// written — an author who wrote the prefix meant it, and interchange documents
// arrive fully qualified — as are function names, keywords, literals and
// anything inside quotes.
//
// The scanner is deliberately small and refuses to be clever: it qualifies only
// what it is certain about, and passing something through unqualified is the
// safe failure (the same SQL as before), not a wrong one.
// ---------------------------------------------------------------------------

// sqlKeywords are words that may appear bare in an expression without being
// column references.
var sqlKeywords = map[string]bool{
	"case": true, "when": true, "then": true, "else": true, "end": true,
	"and": true, "or": true, "not": true, "null": true, "is": true,
	"in": true, "between": true, "like": true, "ilike": true, "escape": true,
	"distinct": true, "cast": true, "as": true, "interval": true,
	"true": true, "false": true, "unknown": true,
	"year": true, "month": true, "day": true, "hour": true, "minute": true, "second": true,
	"filter": true, "where": true, "over": true, "partition": true, "order": true, "by": true,
	"asc": true, "desc": true, "nulls": true, "first": true, "last": true,
	"current_date": true, "current_timestamp": true, "localtime": true, "localtimestamp": true,
}

// token is one identifier found by scanSQL, with the context that decides what
// it is.
type token struct {
	word      string
	qualified bool // preceded by a dot: the tail of a.b
	qualifier bool // followed by a dot: the head of a.b
	call      bool // followed by "(": a function name
	keyword   bool // a bare SQL word that is not a column
}

// bare reports whether the token is a plain column or metric reference — the
// only kind either caller needs to act on.
func (t token) bare() bool {
	return !t.qualified && !t.qualifier && !t.call && !t.keyword
}

// scanSQL walks a SQL fragment and calls emit for every identifier, passing
// through quoted regions untouched: a string literal is not a column, and a
// quoted identifier was spelled out on purpose. rewrite, when non-nil, returns
// the replacement text for a token and the result is returned as a string.
//
// It is deliberately small and refuses to be clever. Both callers treat "I am
// not sure" as "leave it alone", so the failure mode is the SQL that would have
// been emitted anyway, never a different one.
func scanSQL(expr string, emit func(token), rewrite func(token) string) string {
	var b strings.Builder
	runes := []rune(expr)
	n := len(runes)

	for i := 0; i < n; {
		ch := runes[i]
		switch {
		// A comment is not SQL. Rewriting identifiers inside one turns a note
		// the modeller left for the next reader into something that looks like
		// code, and a line comment that swallows a qualified reference changes
		// where the rest of the expression thinks its columns live.
		case ch == '-' && i+1 < n && runes[i+1] == '-':
			j := i
			for j < n && runes[j] != '\n' {
				j++
			}
			b.WriteString(string(runes[i:j]))
			i = j

		case ch == '/' && i+1 < n && runes[i+1] == '*':
			j := i + 2
			for j+1 < n && !(runes[j] == '*' && runes[j+1] == '/') {
				j++
			}
			if j+1 < n {
				j += 2
			} else {
				j = n // unterminated: pass the rest through rather than parse it
			}
			b.WriteString(string(runes[i:j]))
			i = j

		case ch == '\'' || ch == '"' || ch == '`':
			j := i + 1
			for j < n {
				if runes[j] == ch {
					// Doubled quote is an escaped quote, not the end.
					if j+1 < n && runes[j+1] == ch {
						j += 2
						continue
					}
					break
				}
				j++
			}
			if j < n {
				j++
			}
			b.WriteString(string(runes[i:j]))
			i = j

		case isIdentStart(ch):
			j := i
			for j < n && isIdentPart(runes[j]) {
				j++
			}
			word := string(runes[i:j])
			next := nextNonSpaceAt(runes, j)
			t := token{
				word:      word,
				qualified: prevNonSpace(runes, i) == '.',
				qualifier: next == '.',
				call:      next == '(',
				keyword:   sqlKeywords[strings.ToLower(word)],
			}
			if emit != nil {
				emit(t)
			}
			if rewrite != nil {
				b.WriteString(rewrite(t))
			} else {
				b.WriteString(word)
			}
			i = j

		default:
			b.WriteRune(ch)
			i++
		}
	}
	return b.String()
}

// qualifyExpr prefixes every bare column reference in expr with alias, quoted
// by the dialect.
func qualifyExpr(expr, alias string, d Dialect) string {
	return scanSQL(expr, nil, func(t token) string {
		if !t.bare() {
			return t.word
		}
		return d.QuoteIdent(alias) + "." + d.QuoteIdent(t.word)
	})
}

// unknownFormulaRefs returns the bare identifiers in a formula that name no
// metric.
//
// Passing unknown words through is what lets a formula call nullif() or
// coalesce() — but a function call is followed by a parenthesis and a bare word
// is not, and a bare word that names no metric is a typo. Left alone it reaches
// the warehouse as a column reference: usually an error at run time, from
// inside generated SQL, long after the model was reviewed; occasionally it
// resolves against a column that really is in scope, and then it is an answer.
func (m *Model) unknownFormulaRefs(formula string) []string {
	var bad []string
	seen := map[string]bool{}
	scanSQL(formula, func(t token) {
		if !t.bare() || m.Metric(t.word) != nil || seen[t.word] {
			return
		}
		seen[t.word] = true
		bad = append(bad, t.word)
	}, nil)
	return bad
}

func isIdentStart(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isIdentPart(r rune) bool {
	return isIdentStart(r) || (r >= '0' && r <= '9')
}

func prevNonSpace(runes []rune, i int) rune {
	for k := i - 1; k >= 0; k-- {
		if !isSpace(runes[k]) {
			return runes[k]
		}
	}
	return 0
}

func nextNonSpaceAt(runes []rune, i int) rune {
	for k := i; k < len(runes); k++ {
		if !isSpace(runes[k]) {
			return runes[k]
		}
	}
	return 0
}

func isSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }
