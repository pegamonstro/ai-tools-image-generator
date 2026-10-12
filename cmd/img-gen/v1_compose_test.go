package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newV1ComposeServer is newV1Server with a real 8x8 PNG payload: compose
// parents decode child bytes, so the fake-png harness would fail every set.
func newV1ComposeServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	real := image.NewRGBA(image.Rect(0, 0, 8, 8))
	var buf bytes.Buffer
	if err := png.Encode(&buf, real); err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString(buf.Bytes())
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/images/generations":
			w.Write([]byte(`{"data":[{"b64_json":"` + b64 + `","seed":7}]}`))
		case "/v1/chat/completions":
			w.Write([]byte(`{"choices":[{"message":{"content":"enhanced prompt"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(mock.Close)
	cfg := config{
		LatticeURL:   mock.URL,
		ImageURL:     mock.URL,
		DataDir:      t.TempDir(),
		ExportDir:    t.TempDir(),
		GenresFile:   writeGenres(t),
		ModelsFile:   writeModels(t),
		PresetsFile:  writePresets(t),
		EnhanceModel: "flux-dev",
		ImageTimeout: 5 * time.Second,
	}
	h, err := newHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, mock.URL
}

func postV1(t *testing.T, srv *httptest.Server, path, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("status %d: %v", resp.StatusCode, err)
	}
	return resp.StatusCode, out
}

const assetSetBody = `{"name":"bolt","seed":1,"assets":[{"kind":"element","name":"bolt","prompt":"hex bolt","size":"16x16"}],"wait_seconds":15}`

func TestV1AssetSetAndWait(t *testing.T) {
	srv, _ := newV1ComposeServer(t)
	_, out := postV1(t, srv, "/api/v1/asset-sets", assetSetBody)
	if out["status"] != "done" {
		t.Fatalf("job: %v", out)
	}
	files, ok := out["files"].([]any)
	if !ok || len(files) != 1 {
		t.Fatalf("files: %v", out["files"])
	}
	f := files[0].(map[string]any)
	if f["name"] != "bolt.png" || f["primary"] != true {
		t.Fatalf("file ref: %v", f)
	}
	if out["set_id"] == "" || out["set_id"] != out["id"] {
		t.Fatalf("set id: %v", out)
	}
}

func TestV1AssetSetNoWait(t *testing.T) {
	srv, _ := newV1ComposeServer(t)
	body := strings.Replace(assetSetBody, `"wait_seconds":15`, `"wait_seconds":0`, 1)
	// The 202 must go out with the JSON Content-Type already set, or Go's
	// sniffer serves the body as text/plain.
	resp, err := http.Post(srv.URL+"/api/v1/asset-sets", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d, want 202", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id, ok := out["job_id"].(string)
	if !ok || id == "" || out["set_id"] != id {
		t.Fatalf("no-wait body: %v", out)
	}
	waitForJobDone(t, srv, id)
}

func TestV1GIFFromSpecAndWait(t *testing.T) {
	srv, _ := newV1ComposeServer(t)
	body := `{"name":"pulse","seed":5,"source":{"type":"from_spec","request":{"mode":"asset","prompt":"pulse orb","size":"16x16","seed":5},"count":3},"wait_seconds":15}`
	_, out := postV1(t, srv, "/api/v1/gifs", body)
	if out["status"] != "done" {
		t.Fatalf("job: %v", out)
	}
}

func TestV1ComposeValidationEnvelope(t *testing.T) {
	srv, _ := newV1ComposeServer(t)
	// asset set without seed → 400 validation_error
	code, out := postV1(t, srv, "/api/v1/asset-sets", `{"name":"x","assets":[]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status %d", code)
	}
	e := out["error"].(map[string]any)
	if e["code"] != "validation_error" {
		t.Fatalf("code: %v", e)
	}
	// keyframes gif → 400 not_implemented
	code, out = postV1(t, srv, "/api/v1/gifs", `{"name":"k","source":{"type":"keyframes","count":4}}`)
	if code != http.StatusBadRequest || out["error"].(map[string]any)["code"] != "not_implemented" {
		t.Fatalf("keyframes: %d %v", code, out)
	}
}

func TestV1SetGet(t *testing.T) {
	srv, _ := newV1ComposeServer(t)
	_, out := postV1(t, srv, "/api/v1/asset-sets", assetSetBody)
	if out["status"] != "done" {
		t.Fatalf("job: %v", out)
	}
	setID := out["set_id"].(string)
	resp, err := http.Get(srv.URL + "/api/v1/sets/" + setID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["set_id"] != setID {
		t.Fatalf("set_id: %v", body["set_id"])
	}
	job := body["job"].(map[string]any)
	if job["status"] != "done" || job["mode"] != "compose" {
		t.Fatalf("job: %v", job)
	}
	files := body["files"].([]any)
	if len(files) != 1 || files[0].(map[string]any)["name"] != "bolt.png" {
		t.Fatalf("files: %v", files)
	}
}

func TestV1SetNotFound(t *testing.T) {
	srv, _ := newV1ComposeServer(t)
	code, out := postGet(t, srv, "/api/v1/sets/0000000000000000")
	if code != http.StatusNotFound {
		t.Fatalf("unknown id: %d", code)
	}
	if out["error"].(map[string]any)["code"] != "job_not_found" {
		t.Fatalf("code: %v", out)
	}
	// a non-compose job id must not read as a set
	sub := v1Submit(t, srv, `{"mode":"asset","prompt":"plain","size":"64x64","seed":1}`, "")
	plain := sub["job_id"].(string)
	waitForJobDone(t, srv, plain)
	code, out = postGet(t, srv, "/api/v1/sets/"+plain)
	if code != http.StatusNotFound {
		t.Fatalf("non-compose set: %d", code)
	}
}

func TestV1SetFileServe(t *testing.T) {
	srv, _ := newV1ComposeServer(t)
	_, out := postV1(t, srv, "/api/v1/asset-sets", assetSetBody)
	if out["status"] != "done" {
		t.Fatalf("job: %v", out)
	}
	setID := out["set_id"].(string)
	func() {
		resp, err := http.Get(srv.URL + "/api/v1/sets/" + setID + "/files/bolt.png")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" {
			t.Fatalf("png: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		if _, err := png.Decode(resp.Body); err != nil {
			t.Fatalf("served body not a png: %v", err)
		}
	}()
	// manifest.json serves as json
	func() {
		resp, err := http.Get(srv.URL + "/api/v1/sets/" + setID + "/files/manifest.json")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("json content type: %s", resp.Header.Get("Content-Type"))
		}
	}()
	// download param adds the disposition header
	func() {
		resp, err := http.Get(srv.URL + "/api/v1/sets/" + setID + "/files/bolt.png?download=1")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get("Content-Disposition") == "" {
			t.Fatal("download=1 must set Content-Disposition")
		}
	}()
	// missing file, name with ".." (the Task 5 rule reaches HTTP), bad set id
	for _, p := range []string{"/files/nope.png", "/files/x..y.png"} {
		resp, err := http.Get(srv.URL + "/api/v1/sets/" + setID + p)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("expected 404 for %s", p)
		}
		resp.Body.Close()
	}
	resp, err := http.Get(srv.URL + "/api/v1/sets/not-hex/files/bolt.png")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bad set id: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// postGet issues a GET and decodes the envelope-shaped body regardless of
// status, so 404 tests can assert on the error envelope.
func postGet(t *testing.T, srv *httptest.Server, path string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("status %d: %v", resp.StatusCode, err)
	}
	return resp.StatusCode, out
}
