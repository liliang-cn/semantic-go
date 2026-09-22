package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// The CLI is how most people meet this package, and CI depends on its exit
// codes — `semc lint` failing a build is the whole point of the gate. These
// cover the contract: what exits non-zero, and what the commands actually emit.

// capture redirects the command output writers, runs fn, and returns what it
// printed. No os.Stdout swapping: that races with anything else writing, and
// nothing at the call site shows it is happening.
func capture(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	var out, errs bytes.Buffer
	savedOut, savedErr := stdout, stderr
	stdout, stderr = &out, &errs
	defer func() { stdout, stderr = savedOut, savedErr }()
	err := fn()
	return out.String() + errs.String(), err
}

const model = "../../testdata/shifts.yaml"

func TestCompileCommand(t *testing.T) {
	out, err := capture(t, func() error {
		return cmdCompile([]string{"-model", model, "-metrics", "total_shift_hours", "-by", "guard_region", "-dialect", "duckdb"})
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, want := range []string{"WITH", "SUM(", "GROUP BY", ";"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	// Synonyms resolve before compiling, so a natural-language name works.
	if _, err := capture(t, func() error {
		return cmdCompile([]string{"-model", model, "-metrics", "labour cost", "-by", "region", "-dialect", "duckdb"})
	}); err != nil {
		t.Errorf("synonyms should resolve on the command line: %v", err)
	}
}

func TestCompileCommandRefusals(t *testing.T) {
	cases := []struct {
		name, want string
		args       []string
	}{
		{"no metrics", "-metrics is required", []string{"-model", model}},
		{"unknown dialect", "unknown dialect", []string{"-model", model, "-metrics", "total_shift_hours", "-dialect", "oracle"}},
		{"unknown metric", "did you mean", []string{"-model", model, "-metrics", "total_shift_hour"}},
		{"a refusal from the compiler surfaces", "no declared join path",
			[]string{"-model", model, "-metrics", "total_shift_hours", "-by", "client_site_category"}},
		{"role-restricted metric without the role", "requires one of roles",
			[]string{"-model", model, "-metrics", "total_invoiced", "-by", "client_name"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := capture(t, func() error { return cmdCompile(tc.args) })
			if err == nil {
				t.Fatal("expected an error, which is what makes the exit code non-zero")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want %q, got: %v", tc.want, err)
			}
		})
	}
	// The same metric with the role compiles.
	if _, err := capture(t, func() error {
		return cmdCompile([]string{"-model", model, "-metrics", "total_invoiced", "-by", "client_name", "-roles", "finance"})
	}); err != nil {
		t.Errorf("finance should be admitted: %v", err)
	}
}

// CI runs this and depends on the exit code: a model that would refuse queries
// at run time must fail the build instead.
func TestLintCommandExitCode(t *testing.T) {
	if _, err := capture(t, func() error { return cmdLint([]string{"-model", model}) }); err != nil {
		t.Errorf("the bundled model should pass its own gate: %v", err)
	}

	broken := filepathJoin(t.TempDir(), "broken.yaml")
	writeFile(t, broken, `
name: broken
description: a model with a metric that names nothing
entities:
  - {name: e, table: t, primary_key: id, grain_status: pass}
dimensions:
  - {name: d, entity: e, column: c, type: categorical}
metrics:
  - {name: a, description: x, synonyms: [aa], entity: e, agg: sum, expr: v}
  - {name: b, description: x, synonyms: [bb], formula: "a - ghost"}
`)
	out, err := capture(t, func() error { return cmdLint([]string{"-model", broken}) })
	if err == nil {
		t.Fatal("a model with a broken formula must fail the gate")
	}
	if !strings.Contains(out, "ghost") {
		t.Errorf("the gate should name the offending reference:\n%s", out)
	}
}

func TestGrainCommand(t *testing.T) {
	out, err := capture(t, func() error { return cmdGrain([]string{"-model", model, "-dialect", "duckdb"}) })
	if err != nil {
		t.Fatal(err)
	}
	// The composite grain must be asserted on every column, or the check
	// passes while the grain is a lie.
	if !strings.Contains(out, `GROUP BY "guard_id", "shift_id"`) {
		t.Errorf("composite grain not asserted in full:\n%s", out)
	}
	if !strings.Contains(out, "HAVING COUNT(*) > 1") {
		t.Errorf("the assertion should return the duplicates:\n%s", out)
	}
}

func TestMetricsAndDimensionsCommands(t *testing.T) {
	out, err := capture(t, func() error { return cmdMetrics([]string{"-model", model}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"native_dataset"`) || !strings.Contains(out, `"grain"`) {
		t.Errorf("the catalog should carry the dataset and its grain:\n%s", out)
	}
	// A role-restricted metric is omitted rather than listed and then refused.
	if strings.Contains(out, "total_invoiced") {
		t.Error("a restricted metric should not be listed to a role-less caller")
	}

	out, err = capture(t, func() error {
		return cmdDimensions([]string{"-model", model, "-metrics", "total_shift_hours,total_shift_cost"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"unavailable"`) || !strings.Contains(out, "site_assignment") {
		t.Errorf("the exclusion list and its reason are what stop a caller hand-rolling SQL:\n%s", out)
	}
	if _, err := capture(t, func() error { return cmdDimensions([]string{"-model", model}) }); err == nil {
		t.Error("-metrics is required")
	}
}

func TestDomainsCommandAndDirectoryLoading(t *testing.T) {
	out, err := capture(t, func() error { return cmdDomains([]string{"-model", "../../testdata"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"meridian_retail", "exec_security_shifts", "metric_count"} {
		if !strings.Contains(out, want) {
			t.Errorf("routing catalog missing %q:\n%s", want, out)
		}
	}
	// A directory publishes several domains, and which one is not a guess.
	if _, err := capture(t, func() error { return cmdMetrics([]string{"-model", "../../testdata"}) }); err == nil {
		t.Error("an ambiguous domain must be refused")
	}
	if _, err := capture(t, func() error {
		return cmdMetrics([]string{"-model", "../../testdata", "-domain", "exec_security_shifts"})
	}); err != nil {
		t.Errorf("naming the domain should work: %v", err)
	}
}

func TestImportCommand(t *testing.T) {
	out, err := capture(t, func() error {
		return cmdImport([]string{"-dbt", "../../testdata/dbt/semantic_manifest.json", "-emit"})
	})
	if err != nil {
		t.Fatalf("dbt import: %v", err)
	}
	// -emit exists to be reviewed, so it has to be readable: the converted
	// model, not a wall of empty fields.
	if strings.Contains(out, `mask: ""`) || strings.Contains(out, "synonyms: []") {
		t.Errorf("-emit should omit empty fields:\n%s", out)
	}
	if !strings.Contains(out, "cardinality: many_to_one") {
		t.Errorf("the emitted model should show the inferred cardinality:\n%s", out)
	}

	if _, err := capture(t, func() error { return cmdImport([]string{"-ossie", "../../testdata/ossie/shifts_ossie.yaml"}) }); err != nil {
		t.Errorf("interchange import: %v", err)
	}
	for _, args := range [][]string{
		{},
		{"-dbt", "x", "-ossie", "y"},
	} {
		if _, err := capture(t, func() error { return cmdImport(args) }); err == nil {
			t.Errorf("args %v should be refused", args)
		}
	}
}

func TestCubeFromStdin(t *testing.T) {
	body := `{"measures":["total_shift_hours"],"dimensions":["guard_region"],
	  "filters":[{"member":"shift_type","operator":"equals","values":["night"]}]}`
	saved := stdin
	stdin = strings.NewReader(body)
	defer func() { stdin = saved }()

	out, err := capture(t, func() error {
		return cmdCompile([]string{"-model", model, "-cube", "-", "-dialect", "bigquery"})
	})
	if err != nil {
		t.Fatalf("cube from stdin: %v", err)
	}
	if !strings.Contains(out, "`m_total_shift_hours`") {
		t.Errorf("expected BigQuery-quoted SQL:\n%s", out)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func filepathJoin(dir, name string) string { return dir + string(os.PathSeparator) + name }

// The mirror of list_dimensions: pick the breakdown first, and see which
// numbers can honestly be shown against it.
func TestMetricsByBreakdown(t *testing.T) {
	out, err := capture(t, func() error {
		return cmdMetrics([]string{"-model", model, "-by", "guard_region", "-roles", "finance"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "total_shift_hours") || !strings.Contains(out, "total_shift_cost") {
		t.Errorf("both measures conform onto region:\n%s", out)
	}
	// total_invoiced shares no dimension with shifts, so it is named as
	// unavailable rather than silently absent.
	if !strings.Contains(out, "total_invoiced") || !strings.Contains(out, "no declared join path") {
		t.Errorf("the exclusion and its reason should be reported:\n%s", out)
	}

	// A breakdown nothing supports returns an empty list plus the reasons —
	// not an error, and not a bare empty list either.
	out, err = capture(t, func() error {
		return cmdMetrics([]string{"-model", model, "-by", "client_site_category"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "site_assignment") {
		t.Errorf("the bridge should be named as the reason:\n%s", out)
	}

	// An unknown breakdown must not read as "no metric supports it".
	if _, err := capture(t, func() error {
		return cmdMetrics([]string{"-model", model, "-by", "nonexistent"})
	}); err == nil {
		t.Error("an unknown dimension should be an error, not an empty result")
	}
}
