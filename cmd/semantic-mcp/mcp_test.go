package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	semantic "github.com/liliang-cn/semantic-go"
)

// The transport here is hand-written rather than taken from the SDK, which is
// only a defensible trade if it is actually exercised: the handshake, the
// notification that takes no reply, the unknown method, and — the one that
// matters most — a tool refusal arriving as a readable result rather than as a
// protocol error.

func newTestHandler(t *testing.T) *handler {
	t.Helper()
	cat, err := semantic.LoadCatalog("../../testdata/shifts.yaml")
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	return &handler{catalog: cat, dialect: semantic.BigQuery{}}
}

// A server publishing several domains routes first, and refuses to guess which
// one an un-namespaced question meant.
func newMultiDomainHandler(t *testing.T) *handler {
	t.Helper()
	cat, err := semantic.LoadCatalog("../../testdata")
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	return &handler{catalog: cat, dialect: semantic.DuckDB{}}
}

// session runs a list of requests through a server and returns the replies.
func session(t *testing.T, h *handler, lines ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	srv := newServer(&out, "semantic-go", "test")
	h.register(srv)
	if err := srv.serve(strings.NewReader(strings.Join(lines, "\n") + "\n")); err != nil {
		t.Fatalf("serve: %v", err)
	}
	var replies []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("reply is not JSON: %q", line)
		}
		replies = append(replies, v)
	}
	return replies
}

func result(t *testing.T, reply map[string]any) map[string]any {
	t.Helper()
	r, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", reply)
	}
	return r
}

func structured(t *testing.T, reply map[string]any) map[string]any {
	t.Helper()
	sc, ok := result(t, reply)["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("no structuredContent in %v", reply)
	}
	return sc
}

func TestHandshakeAndDiscovery(t *testing.T) {
	h := newTestHandler(t)
	replies := session(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	)
	// The notification must produce no reply at all — a reply to a
	// notification is a protocol violation some clients disconnect over.
	if len(replies) != 2 {
		t.Fatalf("want 2 replies (the notification takes none), got %d: %v", len(replies), replies)
	}
	init := result(t, replies[0])
	// A known version is echoed back rather than upgraded: a tools-only server
	// behaves identically across all of them, so agreeing with the client is
	// both honest and the most compatible answer.
	if got := init["protocolVersion"]; got != "2025-03-26" {
		t.Errorf("protocolVersion = %v, want the client's own", got)
	}
	if _, ok := init["capabilities"].(map[string]any)["tools"]; !ok {
		t.Error("server must advertise the tools capability")
	}

	tools := result(t, replies[1])["tools"].([]any)
	var names []string
	for _, x := range tools {
		names = append(names, x.(map[string]any)["name"].(string))
	}
	for _, want := range []string{"list_metrics", "list_dimensions", "describe_grain", "query_metric"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("tool %q missing from %v", want, names)
		}
	}
}

func TestUnknownProtocolVersionFallsBackToOurs(t *testing.T) {
	h := newTestHandler(t)
	replies := session(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01","capabilities":{}}}`)
	if got := result(t, replies[0])["protocolVersion"]; got != protocolVersions[0] {
		t.Errorf("protocolVersion = %v, want our newest (%s) rather than a guess at theirs", got, protocolVersions[0])
	}
}

// A metric named by synonym resolves, and the dimension list is the
// intersection plus the pre-emptive exclusions.
func TestListDimensionsOverTheWire(t *testing.T) {
	h := newTestHandler(t)
	replies := session(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_dimensions",`+
			`"arguments":{"metrics":["hours worked","labour cost"]}}}`)
	sc := structured(t, replies[0])

	var canonical []string
	for _, x := range sc["metrics"].([]any) {
		canonical = append(canonical, x.(string))
	}
	if len(canonical) != 2 || canonical[0] != "total_shift_hours" || canonical[1] != "total_shift_cost" {
		t.Errorf("synonyms did not resolve to canonical names: %v", canonical)
	}

	var dims []string
	for _, x := range sc["dimensions"].([]any) {
		dims = append(dims, x.(map[string]any)["name"].(string))
	}
	if containsStr(dims, "pay_component") {
		t.Errorf("pay_component is unreachable from the shift fact and must not be offered: %v", dims)
	}
	if !containsStr(dims, "guard_region") {
		t.Errorf("guard_region should be groupable by both measures: %v", dims)
	}
	if len(sc["unavailable"].([]any)) == 0 {
		t.Error("the unavailable list is what stops a caller concluding the model is incomplete")
	}
}

// A refusal is a successful call whose result says no. Returned as a protocol
// error it would read as a tool malfunction, and the model would retry the same
// call instead of reading why it cannot work.
func TestRefusalIsAReadableResultNotATransportError(t *testing.T) {
	h := newTestHandler(t)
	replies := session(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query_metric",`+
			`"arguments":{"measures":["total_shift_hours"],"dimensions":["client_site_category"]}}}`)

	if _, isErr := replies[0]["error"]; isErr {
		t.Fatal("a tool refusal must not be a JSON-RPC error")
	}
	r := result(t, replies[0])
	if r["isError"] != true {
		t.Errorf("result should be flagged isError, got %v", r)
	}
	text := r["content"].([]any)[0].(map[string]any)["text"].(string)
	for _, want := range []string{"no declared join path", "site_assignment", "one-to-many"} {
		if !strings.Contains(text, want) {
			t.Errorf("the refusal must name the path and the offending edge; %q missing from: %s", want, text)
		}
	}
}

// The spec's worked example, end to end over the protocol: hours from one fact
// table, filtered by cost from another at a different grain.
func TestQueryMetricCompilesTheFilterOnlyMeasureCase(t *testing.T) {
	h := newTestHandler(t)
	replies := session(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query_metric","arguments":{`+
			`"measures":["shift.total_shift_hours"],`+
			`"dimensions":["guard.guard_region"],`+
			`"filters":[{"member":"shift.shift_type","operator":"equals","values":["night"]},`+
			`{"member":"shift_pay.total_shift_cost","operator":"gt","values":["50000"]}],`+
			`"timeDimensions":[{"dimension":"shift.shift_date","dateRange":["2026-08-01","2026-08-31"]}]}}}`)
	sc := structured(t, replies[0])

	sql := sc["sql"].(string)
	if !strings.Contains(sql, "`m_total_shift_hours` AS (") || !strings.Contains(sql, "`m_total_shift_cost` AS (") {
		t.Errorf("each measure should aggregate in its own CTE:\n%s", sql)
	}
	if !strings.Contains(sql, "WHERE `m_total_shift_cost`.`total_shift_cost` > ?") {
		t.Errorf("the cost filter should be applied after aggregation, un-coalesced:\n%s", sql)
	}
	if sc["dialect"] != "bigquery" {
		t.Errorf("dialect = %v", sc["dialect"])
	}
	// The wire carries "50000" as a string; a measure compares as a number.
	args := sc["args"].([]any)
	if n, ok := args[len(args)-1].(float64); !ok || n != 50000 {
		t.Errorf("measure filter value should be bound as a number, got %#v", args[len(args)-1])
	}
	if _, ok := sc["provenance"].(map[string]any); !ok {
		t.Error("provenance is what lets an answer be checked; it must be returned")
	}
}

func TestDescribeGrain(t *testing.T) {
	h := newTestHandler(t)
	replies := session(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"describe_grain","arguments":{"dataset":"shift"}}}`)
	sc := structured(t, replies[0])
	grain := sc["grain"].([]any)
	if len(grain) != 2 || grain[0] != "guard_id" || grain[1] != "shift_id" {
		t.Errorf("composite grain lost over the wire: %v", grain)
	}
	if sc["usable"] != true || sc["status"] != "pass" {
		t.Errorf("status = %v, usable = %v", sc["status"], sc["usable"])
	}
}

// A misspelled argument key is a silently unapplied filter unless it is
// rejected, which is the whole reason the decoder is strict.
func TestUnknownArgumentKeyIsRejected(t *testing.T) {
	h := newTestHandler(t)
	replies := session(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query_metric",`+
			`"arguments":{"measures":["total_shift_hours"],"filterz":[]}}}`)
	r := result(t, replies[0])
	if r["isError"] != true {
		t.Fatalf("a misspelled key must not be ignored: %v", r)
	}
}

func TestBadInputHandling(t *testing.T) {
	h := newTestHandler(t)
	replies := session(t, h,
		`not json at all`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"no_such_tool","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"no/such/method"}`,
		`{"jsonrpc":"2.0","method":"no/such/notification"}`,
	)
	if len(replies) != 3 {
		t.Fatalf("want 3 replies (the unknown NOTIFICATION takes none), got %d", len(replies))
	}
	for i, wantCode := range []float64{codeParse, codeMethodNotFound, codeMethodNotFound} {
		e, ok := replies[i]["error"].(map[string]any)
		if !ok {
			t.Errorf("reply %d: want an error, got %v", i, replies[i])
			continue
		}
		if e["code"] != wantCode {
			t.Errorf("reply %d: code = %v, want %v", i, e["code"], wantCode)
		}
	}
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestMultiDomainRouting(t *testing.T) {
	h := newMultiDomainHandler(t)
	if h.catalog.Len() < 2 {
		t.Fatalf("fixture should publish several domains, got %v", h.catalog.Names())
	}

	replies := session(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	var names []string
	for _, x := range result(t, replies[0])["tools"].([]any) {
		names = append(names, x.(map[string]any)["name"].(string))
	}
	if !containsStr(names, "list_domains") {
		t.Errorf("a multi-domain server must offer routing: %v", names)
	}

	// The routing list is a line per domain — no metric names, or a client with
	// fifteen domains spends its context on a question nobody asked yet.
	replies = session(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_domains","arguments":{}}}`)
	domains := structured(t, replies[0])["domains"].([]any)
	if len(domains) != h.catalog.Len() {
		t.Errorf("want %d domains, got %d", h.catalog.Len(), len(domains))
	}
	first := domains[0].(map[string]any)
	for _, field := range []string{"name", "metric_count", "dataset_count"} {
		if _, ok := first[field]; !ok {
			t.Errorf("domain info missing %q: %v", field, first)
		}
	}

	// Omitting the domain is refused, not guessed: a billing question answered
	// out of the rostering model returns a real number about the wrong thing.
	replies = session(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_metrics","arguments":{}}}`)
	r := result(t, replies[0])
	if r["isError"] != true {
		t.Fatalf("an ambiguous domain must be refused: %v", r)
	}
	if text := r["content"].([]any)[0].(map[string]any)["text"].(string); !strings.Contains(text, "name one") {
		t.Errorf("the refusal should list the published domains, got: %s", text)
	}

	// Naming it works.
	replies = session(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_metrics","arguments":{"domain":"exec_security_shifts"}}}`)
	if len(structured(t, replies[0])["metrics"].([]any)) == 0 {
		t.Error("naming the domain should return its catalog")
	}
}

// A single-domain server stays a one-argument call: no routing tool, and the
// domain may be omitted.
func TestSingleDomainNeedsNoRouting(t *testing.T) {
	h := newTestHandler(t)
	replies := session(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	for _, x := range result(t, replies[0])["tools"].([]any) {
		if x.(map[string]any)["name"] == "list_domains" {
			t.Error("a single-domain server should not make the agent route")
		}
	}
	replies = session(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_metrics","arguments":{}}}`)
	if len(structured(t, replies[0])["metrics"].([]any)) == 0 {
		t.Error("the domain should be implicit when there is only one")
	}
}

// list_metrics takes the breakdown too, so a caller that knows it first gets
// the metrics in one call instead of asking list_dimensions per metric and
// intersecting the answers itself.
func TestListMetricsByBreakdown(t *testing.T) {
	h := newTestHandler(t)
	replies := session(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_metrics",`+
			`"arguments":{"by":["client_site_category"]}}}`)
	sc := structured(t, replies[0])
	if len(sc["metrics"].([]any)) != 0 {
		t.Errorf("nothing can be grouped across the bridge: %v", sc["metrics"])
	}
	excl := sc["unavailable"].([]any)
	if len(excl) == 0 {
		t.Fatal("a metric missing without explanation reads as a gap in the model")
	}
	if r := excl[0].(map[string]any)["reason"].(string); !strings.Contains(r, "site_assignment") {
		t.Errorf("the reason should name the edge, got %q", r)
	}

	replies = session(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_metrics",`+
			`"arguments":{"by":["guard_region"],"search":"cost"}}}`)
	sc = structured(t, replies[0])
	var names []string
	for _, x := range sc["metrics"].([]any) {
		names = append(names, x.(map[string]any)["name"].(string))
	}
	if !containsStr(names, "total_shift_cost") {
		t.Errorf("breakdown and search should compose: %v", names)
	}
	if containsStr(names, "total_shift_hours") {
		t.Errorf("search should still narrow: %v", names)
	}

	// An unknown breakdown is a readable refusal, not an empty list.
	replies = session(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_metrics","arguments":{"by":["nope"]}}}`)
	if result(t, replies[0])["isError"] != true {
		t.Error("an unknown dimension must not read as 'no metric supports it'")
	}
}
