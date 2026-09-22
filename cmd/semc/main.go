// Command semc drives the semantic layer from a shell. It opens no database:
// every subcommand either emits SQL for something else to run, or reports on a
// model.
//
//	semc compile -metrics total_revenue -by store_region
//	semc compile -cube query.json -dialect duckdb
//	semc lint                       # the build-time gate; exit 1 on errors
//	semc grain                      # uniqueness assertions for every dataset
//	semc metrics                    # the metric catalog, as JSON
//	semc dimensions -metrics a,b    # what those metrics may be sliced by
//	semc import -ossie doc.yaml     # validate an interchange document
//
// `compile` is the default, so the older flag-only form still works.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	semantic "github.com/liliang-cn/semantic-go"
	"gopkg.in/yaml.v3"
)

// Output goes through these rather than straight to the process's streams, so a
// test can read what a command printed without swapping os.Stdout out from
// under the whole binary — which is both racy and invisible at the call site.
var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
	stdin  io.Reader = os.Stdin
)

func main() {
	args := os.Args[1:]
	cmd := "compile"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	var err error
	switch cmd {
	case "compile":
		err = cmdCompile(args)
	case "lint":
		err = cmdLint(args)
	case "grain":
		err = cmdGrain(args)
	case "metrics":
		err = cmdMetrics(args)
	case "dimensions":
		err = cmdDimensions(args)
	case "import":
		err = cmdImport(args)
	case "domains":
		err = cmdDomains(args)
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(stderr, "semc: unknown command %q\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(stderr, "semc:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(stderr, `semc — compile and check a semantic model

  semc compile     -metrics NAMES [-by DIMS] [-grain G] [-dialect D] [-cube FILE]
  semc lint        report the build-time gate; exit 1 on errors
  semc grain       emit the grain uniqueness assertion for every dataset
  semc metrics     the metric catalog, as JSON (-by DIMS: only what those slice)
  semc dimensions  -metrics NAMES: what those metrics may be sliced by
  semc import      -ossie FILE: validate an interchange document
  semc domains     the routing catalog: one line per published domain

Common flags: -model PATH (a file, or a directory of them), -roles a,b, -dialect NAME
`)
}

// modelFlags registers the flags every subcommand shares and loads the model.
type common struct {
	model   *string
	domain  *string
	roles   *string
	dialect *string
	fs      *flag.FlagSet
}

func newCommon(name string, args []string, extra func(*flag.FlagSet)) (*common, error) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	c := &common{
		model:   fs.String("model", "testdata/meridian.yaml", "a model file, or a DIRECTORY of them — one domain per file"),
		roles:   fs.String("roles", "", "comma-separated caller roles (gates role-restricted metrics)"),
		dialect: fs.String("dialect", "postgres", "SQL dialect: postgres|bigquery|duckdb|snowflake|databricks|mysql|sqlite|sqlserver|ansi"),
		fs:      fs,
	}
	c.domain = fs.String("domain", "", "which published domain to use, when -model names a directory")
	if extra != nil {
		extra(fs)
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return c, nil
}

// load resolves the one model a subcommand operates on. A directory publishes
// several, and which one is not a thing to guess.
func (c *common) load() (*semantic.Model, error) {
	cat, err := c.catalog()
	if err != nil {
		return nil, err
	}
	d, err := cat.Get(*c.domain)
	if err != nil {
		return nil, err
	}
	return d.Model, nil
}

func (c *common) catalog() (*semantic.Catalog, error) { return semantic.LoadCatalog(*c.model) }

func (c *common) dial() (semantic.Dialect, error) {
	d, ok := semantic.DialectByName(*c.dialect)
	if !ok {
		return nil, fmt.Errorf("unknown dialect %q", *c.dialect)
	}
	return d, nil
}

func (c *common) roleList() []string { return splitNonEmpty(*c.roles) }

func cmdCompile(args []string) error {
	var metrics, by, grain, cube, order *string
	var limit, offset *int
	var desc, prov *bool
	c, err := newCommon("compile", args, func(fs *flag.FlagSet) {
		metrics = fs.String("metrics", "", "comma-separated metric names")
		by = fs.String("by", "", "comma-separated group-by dimensions")
		grain = fs.String("grain", "", "time grain for time dimensions (day|week|month|quarter|year)")
		cube = fs.String("cube", "", "path to a Cube-shaped query JSON file (- for stdin); replaces the flags above")
		order = fs.String("order", "", "metric or dimension to order by")
		desc = fs.Bool("desc", false, "order descending")
		limit = fs.Int("limit", 0, "row limit (0 = none)")
		offset = fs.Int("offset", 0, "row offset")
		prov = fs.Bool("provenance", false, "also print the provenance chain to stderr")
	})
	if err != nil {
		return err
	}
	m, err := c.load()
	if err != nil {
		return err
	}
	d, err := c.dial()
	if err != nil {
		return err
	}

	var out semantic.Compiled
	if *cube != "" {
		body, err := readInput(*cube)
		if err != nil {
			return err
		}
		out, err = semantic.CompileCube(m, body, c.roleList(), d)
		if err != nil {
			return err
		}
	} else {
		if *metrics == "" {
			return fmt.Errorf("-metrics is required (or pass -cube FILE)")
		}
		q := semantic.Query{
			Metrics:    splitNonEmpty(*metrics),
			GroupBy:    splitNonEmpty(*by),
			TimeGrain:  *grain,
			OrderBy:    *order,
			Descending: *desc,
			Limit:      *limit,
			Offset:     *offset,
			Roles:      c.roleList(),
		}
		if err := m.ResolveMetrics(&q); err != nil {
			return err
		}
		m.ResolveGroupBy(&q)
		out, err = semantic.Compile(m, q, d)
		if err != nil {
			return err
		}
	}

	fmt.Fprintln(stdout, out.SQL+";")
	if len(out.Args) > 0 {
		fmt.Fprintf(stderr, "-- args: %v\n", out.Args)
	}
	if *prov {
		b, _ := json.MarshalIndent(out.Provenance, "-- ", "  ")
		fmt.Fprintf(stderr, "-- provenance: %s\n", b)
	}
	return nil
}

// cmdLint is the gate a CI job runs. It exits non-zero on errors so a model
// that would refuse queries at run time fails the build instead.
func cmdLint(args []string) error {
	c, err := newCommon("lint", args, nil)
	if err != nil {
		return err
	}
	m, err := c.load()
	if err != nil {
		return err
	}
	issues := semantic.Lint(m)
	errs := 0
	for _, i := range issues {
		fmt.Fprintln(stdout, i)
		if i.Severity == "error" {
			errs++
		}
	}
	if errs > 0 {
		return fmt.Errorf("%d error(s) in %d issue(s)", errs, len(issues))
	}
	fmt.Fprintf(stdout, "ok — %d dataset(s), %d metric(s), %d warning(s)\n", len(m.Entities), len(m.Metrics), len(issues))
	return nil
}

// cmdGrain emits the uniqueness assertion for every dataset. A client without
// dbt has no CI to lean on for this; the assertion is the same either way, and
// anything that speaks SQL can run it.
func cmdGrain(args []string) error {
	c, err := newCommon("grain", args, nil)
	if err != nil {
		return err
	}
	m, err := c.load()
	if err != nil {
		return err
	}
	d, err := c.dial()
	if err != nil {
		return err
	}
	for _, check := range m.GrainCheckSuite(d) {
		fmt.Fprintf(stdout, "-- %s: grain (%s) must be unique; any row returned is a failure\n%s;\n\n",
			check.Entity, strings.Join(check.Grain, ", "), check.SQL)
	}
	return nil
}

// cmdDomains prints the routing catalog — the list an agent keeps resident and
// chooses from before asking anything else.
func cmdDomains(args []string) error {
	c, err := newCommon("domains", args, nil)
	if err != nil {
		return err
	}
	cat, err := c.catalog()
	if err != nil {
		return err
	}
	return printJSON(cat.Domains())
}

func cmdMetrics(args []string) error {
	var meta, by *string
	c, err := newCommon("metrics", args, func(fs *flag.FlagSet) {
		meta = fs.String("meta", "", "require meta tags, e.g. agent_accessible=true,tier=1")
		by = fs.String("by", "", "comma-separated dimensions: keep only metrics sliceable by all of them")
	})
	if err != nil {
		return err
	}
	m, err := c.load()
	if err != nil {
		return err
	}
	f := semantic.MetricFilter{Roles: c.roleList(), Meta: parseKV(*meta), Dimensions: splitNonEmpty(*by)}
	metrics, err := m.ListMetricsBy(f)
	if err != nil {
		return err
	}
	if len(f.Dimensions) == 0 {
		return printJSON(metrics)
	}
	_, excl, err := m.MetricReport(f.Dimensions, f.Roles)
	if err != nil {
		return err
	}
	return printJSON(struct {
		By          []string                   `json:"by"`
		Metrics     []semantic.MetricInfo      `json:"metrics"`
		Unavailable []semantic.MetricExclusion `json:"unavailable"`
	}{f.Dimensions, metrics, excl})
}

func cmdDimensions(args []string) error {
	var metrics *string
	c, err := newCommon("dimensions", args, func(fs *flag.FlagSet) {
		metrics = fs.String("metrics", "", "comma-separated metric names (required)")
	})
	if err != nil {
		return err
	}
	if *metrics == "" {
		return fmt.Errorf("-metrics is required")
	}
	m, err := c.load()
	if err != nil {
		return err
	}
	avail, excl, err := m.DimensionReport(splitNonEmpty(*metrics), c.roleList())
	if err != nil {
		return err
	}
	return printJSON(struct {
		Metrics     []string                   `json:"metrics"`
		Dimensions  []semantic.DimAvailability `json:"dimensions"`
		Unavailable []semantic.DimExclusion    `json:"unavailable"`
	}{splitNonEmpty(*metrics), avail, excl})
}

// cmdImport validates an interchange document and, with -emit, prints the
// native model it converts to — so a conversion can be reviewed before it is
// trusted, rather than taken on faith.
func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	ossie := fs.String("ossie", "", "path to an interchange document (YAML or osi_document.json); - for stdin")
	dbt := fs.String("dbt", "", "path to a dbt semantic_manifest.json; - for stdin")
	domain := fs.String("domain", "", "which semantic model to use (default: all)")
	emit := fs.Bool("emit", false, "print the converted native model as YAML")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if (*ossie == "") == (*dbt == "") {
		return fmt.Errorf("pass exactly one of -ossie or -dbt")
	}
	path := firstNonEmptyArg(*ossie, *dbt)
	body, err := readInput(path)
	if err != nil {
		return err
	}
	var domains []semantic.Domain
	if *dbt != "" {
		domains, err = semantic.ImportDBT(body)
	} else {
		domains, err = semantic.ImportOssie(body)
	}
	if err != nil {
		return err
	}
	for _, d := range domains {
		if *domain != "" && d.Name != *domain {
			continue
		}
		errs := semantic.LintErrors(d.Model)
		fmt.Fprintf(stdout, "%s: %d dataset(s), %d metric(s), %d join(s), %d lint error(s)\n",
			d.Name, len(d.Model.Entities), len(d.Model.Metrics), len(d.Model.Joins), len(errs))
		for _, e := range errs {
			fmt.Fprintln(stdout, " ", e)
		}
		if *emit {
			b, err := yaml.Marshal(d.Model)
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "---\n%s\n", b)
		}
		if len(errs) > 0 {
			return fmt.Errorf("%s: model does not pass the gate", d.Name)
		}
	}
	return nil
}

func firstNonEmptyArg(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func readInput(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(stdin)
	}
	return os.ReadFile(path)
}

func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, string(b))
	return nil
}

func parseKV(s string) map[string]string {
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(part), "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
