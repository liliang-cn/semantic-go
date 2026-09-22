// Command semantic-mcp exposes a semantic model to an agent over MCP (stdio).
//
// It is the lookup/execution split the governed-query pattern rests on: three
// tools that say what exists and at what grain, and one that compiles a
// structured request into grain-safe SQL. The agent never writes SQL and never
// names a table; it picks from a validated menu.
//
//	semantic-mcp -model model.yaml -dialect bigquery
//	semantic-mcp -model model.yaml -ossie osi_document.json -domain exec_security_shifts
//
// It opens no database. query_metric returns SQL and ordered bind arguments for
// the caller's own driver to run — so the same server works against BigQuery,
// a self-hosted Postgres, or a DuckDB file, and the grain guarantees do not
// change with the deployment.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	semantic "github.com/liliang-cn/semantic-go"
)

var stderr = os.Stderr

const version = "0.1.0"

func main() {
	var (
		modelPath = flag.String("model", "", "a model file, or a DIRECTORY of them — one published domain per file")
		ossiePath = flag.String("ossie", "", "path to an interchange document (osi_document.json or YAML)")
		dbtPath   = flag.String("dbt", "", "path to a dbt semantic_manifest.json")
		domain    = flag.String("domain", "", "serve only this domain out of what was loaded")
		dialect   = flag.String("dialect", "postgres", "SQL dialect to compile for")
		roles     = flag.String("roles", "", "comma-separated roles this server's callers hold")
		meta      = flag.String("meta", "", "require meta tags on listed metrics, e.g. agent_accessible=true")
		strict    = flag.Bool("strict", true, "refuse to start if the model fails the build-time gate")
	)
	flag.Parse()

	cat, err := loadCatalog(*modelPath, *ossiePath, *dbtPath, *domain)
	if err != nil {
		fmt.Fprintln(stderr, "semantic-mcp:", err)
		os.Exit(2)
	}
	d, ok := semantic.DialectByName(*dialect)
	if !ok {
		fmt.Fprintf(stderr, "semantic-mcp: unknown dialect %q\n", *dialect)
		os.Exit(2)
	}

	// The gate runs at startup, not per query. A model that would refuse
	// queries at run time should fail to serve, loudly, at a moment somebody is
	// watching — rather than one question at a time, in front of a user.
	failed := 0
	for _, info := range cat.Domains() {
		dom, _ := cat.Get(info.Name)
		for _, i := range semantic.LintErrors(dom.Model) {
			fmt.Fprintf(stderr, "semantic-mcp: %s: %s\n", info.Name, i)
			failed++
		}
	}
	if failed > 0 && *strict {
		fmt.Fprintln(stderr, "semantic-mcp: refusing to serve a model that fails the gate (pass -strict=false to override)")
		os.Exit(2)
	}

	h := &handler{
		catalog: cat,
		dialect: d,
		filter: semantic.MetricFilter{
			Roles: splitNonEmpty(*roles),
			Meta:  parseKV(*meta),
		},
	}
	srv := newServer(os.Stdout, "semantic-go", version)
	h.register(srv)

	for _, info := range cat.Domains() {
		fmt.Fprintf(stderr, "semantic-mcp: %s — %d metric(s), %d dataset(s)\n", info.Name, info.Metrics, info.Datasets)
	}
	fmt.Fprintf(stderr, "semantic-mcp: serving %d domain(s) over %s\n", cat.Len(), d.Name())
	if err := srv.serve(os.Stdin); err != nil {
		fmt.Fprintln(stderr, "semantic-mcp:", err)
		os.Exit(1)
	}
}

func loadCatalog(modelPath, ossiePath, dbtPath, only string) (*semantic.Catalog, error) {
	var cat *semantic.Catalog
	var err error
	given := 0
	for _, p := range []string{modelPath, ossiePath, dbtPath} {
		if p != "" {
			given++
		}
	}
	if given > 1 {
		return nil, fmt.Errorf("pass one of -model, -ossie or -dbt")
	}
	load := func(f func(string) ([]semantic.Domain, error), path string) {
		var domains []semantic.Domain
		if domains, err = f(path); err == nil {
			cat, err = semantic.NewCatalog(domains...)
		}
	}
	switch {
	case modelPath != "":
		cat, err = semantic.LoadCatalog(modelPath)
	case ossiePath != "":
		load(semantic.ImportOssieFile, ossiePath)
	case dbtPath != "":
		load(semantic.ImportDBTFile, dbtPath)
	default:
		return nil, fmt.Errorf("-model, -ossie or -dbt is required (a file, or a directory of them)")
	}
	if err != nil {
		return nil, err
	}
	if only == "" {
		return cat, nil
	}
	d, err := cat.Get(only)
	if err != nil {
		return nil, err
	}
	return semantic.NewCatalog(*d)
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

func decode(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
