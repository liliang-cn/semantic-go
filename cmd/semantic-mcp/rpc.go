package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// ---------------------------------------------------------------------------
// MCP over stdio, by hand.
//
// A tools-only MCP server is newline-delimited JSON-RPC 2.0 on stdin/stdout and
// four methods. Writing those ~200 lines rather than taking the SDK keeps this
// module's dependency set at "stdlib plus a YAML parser", which is a property
// the library advertises and which a client evaluating it will check. The SDK
// is the right call for a server with sampling, resources, prompts or
// subscriptions; none of those appear here.
// ---------------------------------------------------------------------------

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// JSON-RPC reserved codes. Anything a tool itself refuses is NOT one of these:
// a refusal is a successful call whose result says no, so the model reads the
// reason and can act on it, rather than a transport error it can only retry.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInternal       = -32603
)

type server struct {
	out  *json.Encoder
	mu   sync.Mutex // one goroutine writes today, but stdout framing is not a thing to get wrong later
	name string
	ver  string

	tools  []toolDef
	byName map[string]toolDef
}

type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`

	handle func(json.RawMessage) (any, error) `json:"-"`
}

func newServer(w io.Writer, name, version string) *server {
	return &server{out: json.NewEncoder(w), name: name, ver: version, byName: map[string]toolDef{}}
}

func (s *server) register(t toolDef) {
	s.tools = append(s.tools, t)
	s.byName[t.Name] = t
}

// serve reads requests until stdin closes. A client that hangs up mid-session
// is an ordinary end, not an error.
func (s *server) serve(r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			s.fail(nil, codeParse, "invalid JSON: "+err.Error())
			continue
		}
		s.dispatch(req)
	}
	return sc.Err()
}

func (s *server) dispatch(req request) {
	// A notification has no id and takes no reply — including the
	// `notifications/initialized` the handshake ends with.
	isNotification := len(req.ID) == 0

	switch req.Method {
	case "initialize":
		s.initialize(req)
	case "notifications/initialized", "notifications/cancelled":
		// nothing to do, and nothing to say
	case "ping":
		if !isNotification {
			s.reply(req.ID, map[string]any{})
		}
	case "tools/list":
		s.reply(req.ID, map[string]any{"tools": s.tools})
	case "tools/call":
		s.callTool(req)
	default:
		if !isNotification {
			s.fail(req.ID, codeMethodNotFound, "unsupported method "+req.Method)
		}
	}
}

// protocolVersions this server has been written against, newest first.
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

func (s *server) initialize(req request) {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(req.Params, &p)

	// Echo the client's version back when it is one we know. A tools-only
	// server behaves identically across all of them, so agreeing with the
	// client is both honest and the most compatible answer; an unknown version
	// gets our newest rather than a guess at theirs.
	version := protocolVersions[0]
	for _, v := range protocolVersions {
		if p.ProtocolVersion == v {
			version = v
			break
		}
	}
	s.reply(req.ID, map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": s.name, "version": s.ver},
	})
}

func (s *server) callTool(req request) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.fail(req.ID, codeInvalidRequest, "bad params: "+err.Error())
		return
	}
	t, ok := s.byName[p.Name]
	if !ok {
		s.fail(req.ID, codeMethodNotFound, "unknown tool "+p.Name)
		return
	}
	result, err := t.handle(p.Arguments)
	if err != nil {
		// isError, not a transport error. Every refusal this layer makes is
		// designed to be read and acted on — "no declared join path from X to
		// Y" tells a model to pick a different dimension. Returned as a
		// protocol error it would surface as a tool malfunction instead, and
		// the model would retry the same call.
		s.reply(req.ID, map[string]any{
			"isError": true,
			"content": []any{map[string]any{"type": "text", "text": err.Error()}},
		})
		return
	}
	body, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		s.fail(req.ID, codeInternal, err.Error())
		return
	}
	s.reply(req.ID, map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(body)}},
		"structuredContent": result,
	})
}

func (s *server) reply(id json.RawMessage, result any) {
	s.write(response{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *server) fail(id json.RawMessage, code int, msg string) {
	if id == nil {
		id = json.RawMessage("null")
	}
	s.write(response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (s *server) write(v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.out.Encode(v); err != nil {
		fmt.Fprintln(stderr, "semantic-mcp: write:", err)
	}
}

// object is a small helper for the hand-written JSON Schemas below.
func object(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func strField(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func strArray(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}
