// Command semc compiles a semantic query to SQL and prints it.
//
//	semc -model testdata/meridian.yaml -metrics total_revenue -by store_region
//	semc -model testdata/meridian.yaml -metrics net_revenue -by store_region
//	semc -model testdata/meridian.yaml -metrics total_revenue,order_count,avg_order_value
//
// It opens no database — it only emits SQL (pipe it into psql to run).
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	semantic "github.com/liliang-cn/semantic-go"
)

func main() {
	model := flag.String("model", "testdata/meridian.yaml", "path to semantic model YAML")
	metrics := flag.String("metrics", "", "comma-separated metric names (required)")
	by := flag.String("by", "", "comma-separated group-by dimensions")
	grain := flag.String("grain", "", "time grain for time dimensions (day|month|quarter|year)")
	limit := flag.Int("limit", 0, "row limit (0 = none)")
	flag.Parse()

	if *metrics == "" {
		fmt.Fprintln(os.Stderr, "semc: -metrics is required")
		os.Exit(2)
	}
	m, err := semantic.LoadFile(*model)
	if err != nil {
		fmt.Fprintln(os.Stderr, "semc:", err)
		os.Exit(2)
	}
	q := semantic.Query{
		Metrics:   splitNonEmpty(*metrics),
		GroupBy:   splitNonEmpty(*by),
		TimeGrain: *grain,
		Limit:     *limit,
	}
	out, err := semantic.Compile(m, q, semantic.Postgres{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "semc:", err)
		os.Exit(1)
	}
	fmt.Println(out.SQL + ";")
	if len(out.Args) > 0 {
		fmt.Fprintf(os.Stderr, "-- args: %v\n", out.Args)
	}
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
