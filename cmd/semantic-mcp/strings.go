package main

import "strings"

// normalize folds case and treats underscores as spaces, so "shift hours"
// matches "shift_hours" — the difference between how a person asks and how a
// column is named.
func normalize(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "_", " ")
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
