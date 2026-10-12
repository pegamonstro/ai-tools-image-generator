package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newBackend(t *testing.T) (*httptest.Server, *[]string, *string) {
	t.Helper()
	paths := &[]string{}
	genBody := new(string)
	img := []byte("fake-png-bytes")
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*paths = append(*paths, r.Method+" "+r.URL.RequestURI()+" auth="+r.Header.Get("Authorization"))
		if r.URL.Path == "/api/v1/generate" {
			b, _ := io.ReadAll(r.Body)
			*genBody = string(b)
		}
		switch {
		case r.URL.Path == "/api/v1/catalogs":
			w.Write([]byte(`{"genres":{"x":1},"models":[],"presets":[]}`))
		case r.URL.Path == "/api/v1/jobs" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"job_id":"abc123"}`))
		case r.URL.Path == "/api/v1/generate":
			w.Write([]byte(`{"id":"abc123","status":"done","prompt":"final"}`))
		case r.URL.Path == "/api/v1/jobs/abc123":
			w.Write([]byte(`{"id":"abc123","status":"done"}`))
		case r.URL.Path == "/api/v1/jobs/abc123/cancel":
			w.Write([]byte(`{"status":"cancelling"}`))
		case r.URL.Path == "/api/v1/jobs":
			w.Write([]byte(`[{"id":"a"},{"id":"b"},{"id":"c"},{"id":"d"}]`))
		case strings.HasPrefix(r.URL.Path, "/api/v1/images/") && strings.HasSuffix(r.URL.Path, ".png"):
			w.Write(img)
		case r.URL.Path == "/api/v1/jobs/bad":
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":{"code":"job_not_found","message":"job not found","retryable":false}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(mock.Close)
	return mock, paths, genBody
}

// roundTrip feeds JSON-RPC lines through serve() and returns the responses.
func roundTrip(t *testing.T, c mcpcfg, lines ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	serve(c, strings.NewReader(strings.Join(lines, "\n")+"\n"), &out)
	split := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(split) != len(lines) {
		t.Fatalf("got %d responses, want %d: %q", len(split), len(lines), out.String())
	}
	resp := make([]map[string]any, len(split))
	for i, s := range split {
		if err := json.Unmarshal([]byte(s), &resp[i]); err != nil {
			t.Fatalf("response %d not JSON: %q", i, s)
		}
	}
	return resp
}

func TestInitialize(t *testing.T) {
	mock, _, _ := newBackend(t)
	r := roundTrip(t, mcpcfg{baseURL: mock.URL}, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)[0]
	if r["jsonrpc"] != "2.0" {
		t.Fatalf("jsonrpc = %v", r["jsonrpc"])
	}
	res := r["result"].(map[string]any)
	if res["protocolVersion"] != protocolVersion {
		t.Fatalf("protocolVersion = %v", res["protocolVersion"])
	}
	info := res["serverInfo"].(map[string]any)
	if info["name"] != serverName || info["version"] != serverVersion {
		t.Fatalf("serverInfo = %v", info)
	}
	if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatalf("capabilities missing tools: %v", res["capabilities"])
	}
}

func TestPingAndUnknownMethod(t *testing.T) {
	mock, _, _ := newBackend(t)
	resp := roundTrip(t, mcpcfg{baseURL: mock.URL},
		`{"jsonrpc":"2.0","id":7,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":8,"method":"no/such"}`)
	if len(resp[0]["result"].(map[string]any)) != 0 {
		t.Fatalf("ping result = %v", resp[0]["result"])
	}
	e := resp[1]["error"].(map[string]any)
	if e["code"] != float64(errMethod) {
		t.Fatalf("unknown method error = %v", e)
	}
}

func TestNotifyConsumedSilently(t *testing.T) {
	mock, _, _ := newBackend(t)
	// A notification (no id) must produce no response line.
	var out bytes.Buffer
	serve(mcpcfg{baseURL: mock.URL},
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`), &out)
	if strings.TrimSpace(out.String()) != "" {
		t.Fatalf("notification produced response: %q", out.String())
	}
}

func TestParseError(t *testing.T) {
	mock, _, _ := newBackend(t)
	resp := roundTrip(t, mcpcfg{baseURL: mock.URL}, `not-json`)
	e := resp[0]["error"].(map[string]any)
	if e["code"] != float64(errParse) {
		t.Fatalf("parse error = %v", e)
	}
}

func TestToolsListShapes(t *testing.T) {
	mock, _, _ := newBackend(t)
	resp := roundTrip(t, mcpcfg{baseURL: mock.URL}, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	ls := resp[0]["result"].(map[string]any)["tools"].([]any)
	if len(ls) != len(tools) {
		t.Fatalf("tools = %d, want %d", len(ls), len(tools))
	}
	for _, it := range ls {
		tool := it.(map[string]any)
		if tool["name"] == "" || tool["description"] == "" {
			t.Fatalf("tool missing name/description: %v", tool)
		}
		sch := tool["inputSchema"].(map[string]any)
		if sch["type"] != "object" {
			t.Fatalf("schema missing object type: %v", tool)
		}
	}
}

func TestToolsCallDispatch(t *testing.T) {
	mock, paths, _ := newBackend(t)
	c := mcpcfg{baseURL: mock.URL, token: "t0k"}
	req := func(name, args string) map[string]any {
		resp := roundTrip(t, c, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
		return resp[0]["result"].(map[string]any)
	}

	// submit_job
	r := req("submit_job", `{"request":{"genre":"landscape","fields":{"setting":"a valley"},"size":"512x512"}}`)
	text := r["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"job_id":"abc123"`) {
		t.Fatalf("submit text = %q", text)
	}
	if r["isError"] == true {
		t.Fatal("submit isError")
	}

	// get_job
	r = req("get_job", `{"job_id":"abc123"}`)
	text = r["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"status":"done"`) {
		t.Fatalf("get_job text = %q", text)
	}

	// list_jobs with limit
	r = req("list_jobs", `{"limit":2}`)
	text = r["content"].([]any)[0].(map[string]any)["text"].(string)
	if strings.Count(text, "\"id\"") != 2 {
		t.Fatalf("list_jobs text = %q", text)
	}

	// generate_and_wait
	r = req("generate_and_wait", `{"request":{"genre":"landscape","fields":{"setting":"a valley"},"size":"512x512"},"wait_seconds":5}`)
	text = r["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"status":"done"`) {
		t.Fatalf("generate_and_wait text = %q", text)
	}

	// cancel_job
	r = req("cancel_job", `{"job_id":"abc123"}`)
	text = r["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "cancelling") {
		t.Fatalf("cancel text = %q", text)
	}

	// get_image returns an image content item.
	r = req("get_image", `{"id":"abc123"}`)
	c1 := r["content"].([]any)[0].(map[string]any)
	if c1["type"] != "image" || c1["mimeType"] != "image/png" {
		t.Fatalf("image content = %v", c1)
	}
	data, _ := base64.StdEncoding.DecodeString(c1["data"].(string))
	if string(data) != "fake-png-bytes" {
		t.Fatalf("image data = %q", data)
	}

	// REST failure surfaces as isError with the envelope text.
	r = req("get_job", `{"job_id":"bad"}`)
	if r["isError"] != true {
		t.Fatal("no isError on REST failure")
	}
	text = r["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "job_not_found") {
		t.Fatalf("error text = %q", text)
	}

	// Missing argument is an isError result too.
	r = req("get_job", `{}`)
	if r["isError"] != true {
		t.Fatalf("missing-arg result = %v", r)
	}

	// Bearer token reached the backend on every call.
	for _, p := range *paths {
		if !strings.Contains(p, "auth=Bearer t0k") {
			t.Fatalf("missing auth header: %q", p)
		}
	}
}

func TestToolsCallUnknownTool(t *testing.T) {
	mock, _, _ := newBackend(t)
	resp := roundTrip(t, mcpcfg{baseURL: mock.URL},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wat","arguments":{}}}`)
	if resp[0]["error"] != nil {
		t.Fatalf("protocol error: %v", resp[0]["error"])
	}
	r := resp[0]["result"].(map[string]any)
	if r["isError"] != true {
		t.Fatalf("result = %v", r)
	}
}

func TestGenerateAndWaitDefaultWait(t *testing.T) {
	// wait_seconds omitted → the REST body carried 900.
	mock, _, genBody := newBackend(t)
	resp := roundTrip(t, mcpcfg{baseURL: mock.URL},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"generate_and_wait","arguments":{"request":{"genre":"landscape"}}}}`)
	if resp[0]["error"] != nil {
		t.Fatalf("error = %v", resp[0]["error"])
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(*genBody), &body); err != nil {
		t.Fatalf("generate body %q: %v", *genBody, err)
	}
	if body["wait_seconds"] != float64(defaultWaitSecs) {
		t.Fatalf("wait_seconds = %v, want %d", body["wait_seconds"], defaultWaitSecs)
	}
	if body["genre"] != "landscape" {
		t.Fatalf("request fields not merged into body: %v", body)
	}
}

// composeMock backs a backend with the compose REST surface and a real PNG
// for previews. It returns the mock and the last asset-set body it saw.
func composeMock(t *testing.T) (*httptest.Server, *string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	seen := new(string)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/asset-sets":
			b, _ := io.ReadAll(r.Body)
			*seen = string(b)
			w.Write([]byte(`{"id":"set0001","set_id":"set0001","status":"done","files":[{"name":"bolt.png","primary":true}]}`))
		case r.URL.Path == "/api/v1/gifs":
			b, _ := io.ReadAll(r.Body)
			*seen = string(b)
			w.Write([]byte(`{"id":"set0002","set_id":"set0002","status":"done","files":[{"name":"spin.gif","primary":true}]}`))
		case r.URL.Path == "/api/v1/sets/set0001":
			w.Write([]byte(`{"set_id":"set0001","job":{"id":"set0001","status":"done"},"files":[{"name":"bolt.png","primary":true},{"name":"walk-atlas.json","primary":false}]}`))
		case r.URL.Path == "/api/v1/sets/set0001/files/bolt.png":
			w.Write(buf.Bytes())
		case r.URL.Path == "/api/v1/sets/set0001/files/walk-atlas.json":
			w.Write([]byte(`{"frames":{}}`))
		case r.URL.Path == "/api/v1/sets/set0002/files/spin.gif":
			w.Write(buf.Bytes())
		case strings.HasPrefix(r.URL.Path, "/api/v1/sets/set0001/files/") && strings.HasSuffix(r.URL.Path, "manifest.json"):
			w.Write([]byte(`{"set_id":"set0001","kind":"asset-set"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(mock.Close)
	return mock, seen
}

func callOnce(t *testing.T, mockURL, args string) map[string]any {
	t.Helper()
	resp := roundTrip(t, mcpcfg{baseURL: mockURL},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"request_asset_set","arguments":`+args+`}}`)
	if resp[0]["error"] != nil {
		t.Fatalf("protocol error: %v", resp[0]["error"])
	}
	return resp[0]["result"].(map[string]any)
}

func TestRequestAssetSetTool(t *testing.T) {
	mock, seen := composeMock(t)
	r := callOnce(t, mock.URL, `{"request":{"name":"bolt","seed":1,"assets":[{"kind":"element","name":"bolt","prompt":"hex bolt","size":"16x16"}]}}`)
	cs := r["content"].([]any)
	if len(cs) != 2 {
		t.Fatalf("want text + preview, got %d: %v", len(cs), cs)
	}
	text := cs[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"set_id":"set0001"`) {
		t.Fatalf("text: %s", text)
	}
	img := cs[1].(map[string]any)
	if img["type"] != "image" || img["mimeType"] != "image/png" {
		t.Fatalf("preview: %v", img)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(*seen), &body); err != nil {
		t.Fatal(err)
	}
	if body["wait_seconds"] != float64(defaultWaitSecs) {
		t.Fatalf("default wait: %v", body["wait_seconds"])
	}
	if _, ok := body["name"].(string); !ok || body["name"] != "bolt" {
		t.Fatalf("request passthrough: %v", body)
	}
}

func TestRequestAssetSetNotDone(t *testing.T) {
	// status generating → text only, no preview call
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"set0009","set_id":"set0009","status":"queued"}`))
	}))
	defer mock.Close()
	r := callOnce(t, mock.URL, `{"request":{"name":"x","seed":1,"assets":[]}}`)
	if cs := r["content"].([]any); len(cs) != 1 {
		t.Fatalf("no preview before done: %v", cs)
	}
}

func TestMakeGifTool(t *testing.T) {
	mock, seen := composeMock(t)
	resp := roundTrip(t, mcpcfg{baseURL: mock.URL},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"make_gif","arguments":{"request":{"name":"spin","source":{"type":"from_spec","request":{"mode":"asset","prompt":"p","size":"16x16","seed":5},"count":3}}}}}`)
	res := resp[0]["result"].(map[string]any)
	cs := res["content"].([]any)
	if len(cs) != 2 || cs[1].(map[string]any)["mimeType"] != "image/gif" {
		t.Fatalf("gif preview: %v", cs)
	}
	var body map[string]any
	json.Unmarshal([]byte(*seen), &body)
	if _, ok := body["source"].(map[string]any); !ok {
		t.Fatalf("source passthrough: %v", body)
	}
}

func TestReadExportTool(t *testing.T) {
	mock, _ := composeMock(t)
	resp := roundTrip(t, mcpcfg{baseURL: mock.URL},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_export","arguments":{"set_id":"set0001"}}}`)
	res := resp[0]["result"].(map[string]any)
	cs := res["content"].([]any)
	// set view text, bolt.png image, walk-atlas.json text, manifest.json text
	if len(cs) != 4 {
		t.Fatalf("content blocks: %v", cs)
	}
	want := []string{"text", "image", "text", "text"}
	for i, w := range want {
		if got := cs[i].(map[string]any)["type"]; got != w {
			t.Fatalf("block %d: got %v want %s", i, got, w)
		}
	}
	manifest := cs[3].(map[string]any)["text"].(string)
	if !strings.Contains(manifest, `"kind":"asset-set"`) {
		t.Fatalf("manifest text: %s", manifest)
	}
}
