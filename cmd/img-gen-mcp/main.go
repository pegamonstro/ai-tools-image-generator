// img-gen-mcp is a Model Context Protocol (stdio) server that exposes the
// img-gen /api/v1 surface as tools. It speaks JSON-RPC 2.0 over newline-
// delimited JSON on stdin/stdout (the MCP stdio framing) and wraps img-gen's
// REST API with no third-party dependencies.
//
// Environment:
//
//	IMG_GEN_URL   base URL of img-gen (default http://127.0.0.1:8099)
//	IMG_GEN_TOKEN bearer token, sent as Authorization on every request
package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const (
	protocolVersion = "2025-03-26"
	serverName      = "img-gen-mcp"
	serverVersion   = "1.0.0"

	errParse        = -32700
	errMethod       = -32601
	errInvalidArgs  = -32602
	errInternal     = -32603
	defaultWaitSecs = 900
)

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

type toolResult struct {
	Content []content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

type tool struct {
	name        string
	description string
	schema      string
}

var tools = []tool{
	{
		name:        "list_catalogs",
		description: "List img-gen catalogs: available genres (with fields and sizes), models, and character presets.",
		schema:      `{"type":"object","properties":{}}`,
	},
	{
		name:        "submit_job",
		description: "Submit an image generation job. Returns {\"job_id\"} (or {\"job_ids\":[...]} when batch > 1). Use get_job to poll status.",
		schema:      `{"type":"object","required":["request"],"properties":{"request":{"type":"object","description":"img-gen SubmitRequest: genre, fields, size, mode (generate|edit|inpaint|outpaint|blend|upscale|pose), prompt, image (base64), mask (base64), strength, images (base64 refs), strengths, model, loras [{name, scale}], style, preset, batch (1-8), enhance, seed, steps, guidance, negative_prompt"}}}`,
	},
	{
		name:        "get_job",
		description: "Fetch an img-gen job by id, including status, final prompt, seed, and image path.",
		schema:      `{"type":"object","required":["job_id"],"properties":{"job_id":{"type":"string"}}}`,
	},
	{
		name:        "list_jobs",
		description: "List recent img-gen jobs, newest first. Optional limit (default 20).",
		schema:      `{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":500}}}`,
	},
	{
		name:        "generate_and_wait",
		description: "Submit an image generation job and block until it finishes, returning the final job (or a timeout error). Default wait is 900 seconds.",
		schema:      `{"type":"object","required":["request"],"properties":{"request":{"type":"object","description":"img-gen SubmitRequest (see submit_job)"},"wait_seconds":{"type":"integer","minimum":1,"maximum":43200,"default":900}}}`,
	},
	{
		name:        "cancel_job",
		description: "Cancel a queued or running img-gen job.",
		schema:      `{"type":"object","required":["job_id"],"properties":{"job_id":{"type":"string"}}}`,
	},
	{
		name:        "get_image",
		description: "Fetch a generated image by job id as base64 PNG.",
		schema:      `{"type":"object","required":["id"],"properties":{"id":{"type":"string"},"download":{"type":"boolean","default":false}}}`,
	},
}

func schemaOf(name string) map[string]any {
	for _, tl := range tools {
		if tl.name == name {
			sch := map[string]any{}
			if err := json.Unmarshal([]byte(tl.schema), &sch); err != nil {
				return map[string]any{"type": "object"}
			}
			return sch
		}
	}
	return map[string]any{"type": "object"}
}

// ---------------------------------------------------------------- REST client

type mcpcfg struct {
	baseURL string
	token   string
}

func loadCfg() mcpcfg {
	c := mcpcfg{
		baseURL: strings.TrimRight(os.Getenv("IMG_GEN_URL"), "/"),
		token:   os.Getenv("IMG_GEN_TOKEN"),
	}
	if c.baseURL == "" {
		c.baseURL = "http://127.0.0.1:8099"
	}
	return c
}

// do performs one img-gen call. On non-2xx it surfaces the v1 error envelope.
func (c mcpcfg) do(method, path string, body []byte) (status int, raw []byte, err error) {
	req, err := http.NewRequest(method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err = io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, raw, fmt.Errorf("%s", envelopeText(raw, resp.StatusCode))
	}
	return resp.StatusCode, raw, nil
}

// envelopeText renders a v1 error envelope (or plain text) as one human line.
func envelopeText(raw []byte, status int) string {
	var env struct {
		Err struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Err.Code != "" {
		return fmt.Sprintf("%s: %s (http %d)", env.Err.Code, env.Err.Message, status)
	}
	line := strings.TrimSpace(string(raw))
	if len(line) > 300 {
		line = line[:300] + "…"
	}
	return fmt.Sprintf("http %d: %s", status, line)
}

// ---------------------------------------------------------------- tool calls

func callTool(c mcpcfg, name string, args map[string]any) toolResult {
	switch name {
	case "list_catalogs":
		return c.textCall(http.MethodGet, "/api/v1/catalogs", nil)

	case "submit_job":
		req, ok := args["request"].(map[string]any)
		if !ok {
			return errResult("request object is required")
		}
		return c.textCall(http.MethodPost, "/api/v1/jobs", req)

	case "get_job":
		id, _ := args["job_id"].(string)
		if id == "" {
			return errResult("job_id is required")
		}
		return c.textCall(http.MethodGet, "/api/v1/jobs/"+id, nil)

	case "list_jobs":
		raw, err := c.raw(http.MethodGet, "/api/v1/jobs", nil)
		if err != nil {
			return errResult(err.Error())
		}
		var jobs []any
		if err := json.Unmarshal(raw, &jobs); err != nil {
			return errResult("bad catalogs JSON: " + err.Error())
		}
		limit := 20
		if l, ok := args["limit"].(float64); ok && l >= 1 && l <= 500 {
			limit = int(l)
		}
		if len(jobs) > limit {
			jobs = jobs[:limit]
		}
		return textResult(jobs)

	case "generate_and_wait":
		req, ok := args["request"].(map[string]any)
		if !ok {
			return errResult("request object is required")
		}
		body := map[string]any{}
		for k, v := range req {
			body[k] = v
		}
		wait := defaultWaitSecs
		if w, ok := args["wait_seconds"].(float64); ok && w >= 1 {
			wait = int(w)
		}
		body["wait_seconds"] = wait
		return c.textCall(http.MethodPost, "/api/v1/generate", body)

	case "cancel_job":
		id, _ := args["job_id"].(string)
		if id == "" {
			return errResult("job_id is required")
		}
		return c.textCall(http.MethodPost, "/api/v1/jobs/"+id+"/cancel", nil)

	case "get_image":
		id, _ := args["id"].(string)
		if id == "" {
			return errResult("id is required")
		}
		path := "/api/v1/images/" + id + ".png"
		if b, ok := args["download"].(bool); ok && b {
			path += "?download=1"
		}
		raw, err := c.raw(http.MethodGet, path, nil)
		if err != nil {
			return errResult(err.Error())
		}
		return toolResult{Content: []content{{
			Type:     "image",
			Data:     base64.StdEncoding.EncodeToString(raw),
			MimeType: "image/png",
		}}}

	default:
		return errResult("unknown tool " + name)
	}
}

func textResult(v any) toolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return errResult(err.Error())
	}
	return toolResult{Content: []content{{Type: "text", Text: string(b)}}}
}

func errResult(msg string) toolResult {
	return toolResult{Content: []content{{Type: "text", Text: msg}}, IsError: true}
}

func (c mcpcfg) raw(method, path string, body any) ([]byte, error) {
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	_, raw, err := c.do(method, path, b)
	return raw, err
}

func (c mcpcfg) textCall(method, path string, body any) toolResult {
	raw, err := c.raw(method, path, body)
	if err != nil {
		return errResult(err.Error())
	}
	// Pass the img-gen JSON through untouched: callers get the exact contract.
	return toolResult{Content: []content{{Type: "text", Text: string(raw)}}}
}

// ---------------------------------------------------------------- protocol

// dispatch answers one JSON-RPC method. tool calls whose REST traffic fails
// come back as isError results, not protocol errors.
func dispatch(c mcpcfg, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{
				"name":    serverName,
				"version": serverVersion,
			},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		ls := make([]map[string]any, 0, len(tools))
		for _, tl := range tools {
			ls = append(ls, map[string]any{
				"name":        tl.name,
				"description": tl.description,
				"inputSchema": schemaOf(tl.name),
			})
		}
		return map[string]any{"tools": ls}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: errInvalidArgs, Message: "bad tools/call params"}
		}
		args := map[string]any{}
		if len(p.Arguments) > 0 {
			if err := json.Unmarshal(p.Arguments, &args); err != nil {
				return nil, &rpcError{Code: errInvalidArgs, Message: "bad arguments"}
			}
		}
		return callTool(c, p.Name, args), nil
	default:
		return nil, &rpcError{Code: errMethod, Message: "unknown method " + method}
	}
}

// serve is the JSON-Lines loop: one JSON-RPC message per stdin line, one
// response per line with an id; notifications are consumed silently.
func serve(c mcpcfg, in io.Reader, out io.Writer) {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	bw := bufio.NewWriter(out)
	defer bw.Flush()

	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			respond(bw, json.RawMessage("null"), nil, &rpcError{Code: errParse, Message: "parse error"})
			continue
		}
		if req.ID == nil {
			continue
		}
		result, rerr := dispatch(c, req.Method, req.Params)
		respond(bw, req.ID, result, rerr)
	}
}

func respond(w *bufio.Writer, id json.RawMessage, result any, rerr *rpcError) {
	resp := response{JSONRPC: "2.0", ID: id, Result: result, Error: rerr}
	b, err := json.Marshal(resp)
	if err != nil {
		// Marshaling a hand-built response cannot fail except from result
		// values that are themselves broken; degrade to an internal error.
		b, _ = json.Marshal(response{
			JSONRPC: "2.0",
			ID:      id,
			Error:   &rpcError{Code: errInternal, Message: "marshal failure"},
		})
	}
	_, _ = w.Write(b)
	_, _ = w.Write([]byte{'\n'})
	_ = w.Flush()
}

func main() {
	serve(loadCfg(), os.Stdin, os.Stdout)
}
