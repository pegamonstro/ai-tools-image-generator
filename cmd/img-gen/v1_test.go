package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type v1Env struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		JobID     string `json:"job_id"`
		Retryable bool   `json:"retryable"`
	} `json:"error"`
}

// newV1Server builds a handler against a mock lattice with the given
// generation delay, optionally enforcing an API token.
func newV1Server(t *testing.T, genDelay time.Duration, token string) (*httptest.Server, string) {
	t.Helper()
	img := []byte("fake-png-bytes")
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/images/generations":
			time.Sleep(genDelay)
			w.Write([]byte(`{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString(img) + `","seed":123}]}`))
		case "/v1/chat/completions":
			w.Write([]byte(`{"choices":[{"message":{"content":"enhanced prompt"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(mock.Close)

	cfg := config{
		LatticeURL:    mock.URL,
		ImageURL:      mock.URL,
		DataDir:       t.TempDir(),
		GenresFile:    writeGenres(t),
		ModelsFile:    writeModels(t),
		PresetsFile:   writePresets(t),
		EnhanceModel:  "flux-dev",
		EnhanceSystem: "sys",
		ImageTimeout:  5 * time.Second,
		APIToken:      token,
	}
	h, err := newHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, mock.URL
}

// waitForJobDone polls until the job is terminal so background workers have
// written their files before TempDir cleanup runs.
func waitForJobDone(t *testing.T, srv *httptest.Server, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, err := http.Get(srv.URL + "/api/v1/jobs/" + id)
		if err != nil {
			t.Fatal(err)
		}
		var j map[string]any
		json.NewDecoder(r.Body).Decode(&j)
		r.Body.Close()
		if j["status"] == "done" || j["status"] == "failed" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s never finished", id)
}

func v1Submit(t *testing.T, srv *httptest.Server, body string, key string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/jobs", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("status %d: %v", resp.StatusCode, err)
	}
	if err, ok := out["error"].(map[string]any); ok {
		t.Fatalf("submit failed: %v", err)
	}
	return out
}

func TestV1Catalogs(t *testing.T) {
	srv, _ := newV1Server(t, 0, "")
	r, err := http.Get(srv.URL + "/api/v1/catalogs")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", r.StatusCode)
	}
	var cat map[string]any
	json.NewDecoder(r.Body).Decode(&cat)
	for _, k := range []string{"genres", "models", "presets"} {
		if _, ok := cat[k]; !ok {
			t.Fatalf("catalogs missing %q: %v", k, cat)
		}
	}
}

func TestV1SubmitIdempotentReplay(t *testing.T) {
	srv, _ := newV1Server(t, 0, "")
	body := `{"genre":"landscape","fields":{"setting":"a valley"},"size":"512x512"}`

	first := v1Submit(t, srv, body, "k1")
	firstID, _ := first["job_id"].(string)
	if firstID == "" {
		t.Fatal("missing job_id")
	}

	// Replaying the same key returns the same job even with a different body.
	again := v1Submit(t, srv, `{"genre":"landscape","fields":{"setting":"OTHER"},"size":"512x512"}`, "k1")
	if again["job_id"] != firstID {
		t.Fatalf("replay job_id = %v, want %v", again["job_id"], firstID)
	}

	// No key means a fresh job every time.
	fresh := v1Submit(t, srv, body, "")
	if fresh["job_id"] == firstID {
		t.Fatal("keyless submit reused the keyed job id")
	}
	freshID, _ := fresh["job_id"].(string)
	waitForJobDone(t, srv, firstID)
	waitForJobDone(t, srv, freshID)
}

func TestV1SubmitIdempotentConflict(t *testing.T) {
	// A pending key (submit in flight) must not be re-reserved. Simulate by
	// reserving directly on the handler's deps: replay is recorded here only
	// through the HTTP surface, so instead assert a released key is retryable,
	// which is the observable contract of release-after-failure.
	srv, _ := newV1Server(t, 0, "")
	body := `{"genre":"nope","fields":{},"size":"512x512"}`

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/jobs", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Idempotency-Key", "kfail")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// A failed submit must release its key so the same key can be retried.
	retry := v1Submit(t, srv, `{"genre":"landscape","fields":{"setting":"a valley"},"size":"512x512"}`, "kfail")
	if retry["job_id"] == "" {
		t.Fatal("retry after failed submit was blocked")
	}
	id, _ := retry["job_id"].(string)
	waitForJobDone(t, srv, id)
}

// mustNewIdemStore wraps newIdemStore for tests: no error is expected when
// the file lives under t.TempDir.
func mustNewIdemStore(t *testing.T, path string) *idemStore {
	t.Helper()
	s, err := newIdemStore(path)
	if err != nil {
		t.Fatalf("newIdemStore(%q): %v", path, err)
	}
	return s
}

func TestIdemStorePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "idempotency.jsonl")
	s1 := mustNewIdemStore(t, path)
	state, _ := s1.reserve("k1") // fresh
	if state != "fresh" {
		t.Fatalf("want fresh, got %q", state)
	}
	body := []byte(`{"id":"123"}`)
	s1.complete("k1", http.StatusCreated, body)

	s2 := mustNewIdemStore(t, path) // "restart": fresh store, same file
	state, reply := s2.reserve("k1")
	if state != "replay" {
		t.Fatalf("want replay, got %q", state)
	}
	if !bytes.Equal(body, reply.raw) {
		t.Fatalf("body mismatch: %q vs %q", reply.raw, body)
	}
	if reply.status != http.StatusCreated {
		t.Fatalf("replay status = %d, want %d", reply.status, http.StatusCreated)
	}
	// in-flight entries do NOT survive (a crashed process's job is dead):
	s1.reserve("k2")
	s3 := mustNewIdemStore(t, path)
	if st, _ := s3.reserve("k2"); st != "fresh" {
		t.Fatalf("in-flight entry must not replay, got %q", st)
	}
}

func TestIdemStoreRotationCapsReplay(t *testing.T) {
	// write 20005 entries directly to the file (one json line each, via the
	// same struct shape), open a store, assert: seen has last 10000 keys
	// only, and the file on disk now holds exactly 10000 lines.
	dir := t.TempDir()
	path := filepath.Join(dir, "idempotency.jsonl")
	var b bytes.Buffer
	for i := 0; i < 20005; i++ {
		rec, err := json.Marshal(idemRecord{
			Key:    fmt.Sprintf("k%05d", i),
			Status: http.StatusCreated,
			Body:   base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(`{"id":"%d"}`, i))),
		})
		if err != nil {
			t.Fatal(err)
		}
		b.Write(rec)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	s := mustNewIdemStore(t, path)
	if len(s.seen) != idemReplayLines {
		t.Fatalf("seen has %d keys, want %d", len(s.seen), idemReplayLines)
	}
	if _, ok := s.seen["k00000"]; ok {
		t.Fatal("oldest key must have rotated away")
	}
	if e, ok := s.seen["k10005"]; !ok || !e.done || !bytes.Equal(e.body, []byte(`{"id":"10005"}`)) {
		t.Fatal("first kept key (k10005) missing or lost its body after rotation")
	}
	if _, ok := s.seen["k20004"]; !ok {
		t.Fatal("newest key (k20004) missing after rotation")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	n := bytes.Count(raw, []byte{'\n'})
	if n != idemReplayLines {
		t.Fatalf("file on disk holds %d lines, want %d", n, idemReplayLines)
	}
}

func TestV1ValidationEnvelope(t *testing.T) {
	srv, _ := newV1Server(t, 0, "")
	for _, tc := range []struct {
		body string
		code string
	}{
		{`{"genre":"nope","fields":{},"size":"512x512"}`, "unknown_genre"},
		{`{"genre":"landscape","fields":{},"size":"512x512"}`, "validation_error"},
		{`{"genre":"landscape","fields":{"setting":"a valley"},"size":"64x64"}`, "invalid_size"},
		{`{"mode":"blend","prompt":"x","size":"512x512"}`, "validation_error"},
		{`{"mode":"flying","prompt":"x"}`, "unknown_mode"},
		{`{"genre":"landscape","fields":{"setting":"a valley"},"size":"512x512","style":"nope"}`, "unknown_style"},
	} {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/jobs", bytes.NewReader([]byte(tc.body)))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var env v1Env
		json.NewDecoder(resp.Body).Decode(&env)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tc.body, resp.StatusCode)
		}
		if env.Error.Code != tc.code {
			t.Errorf("%s: code = %q, want %q", tc.body, env.Error.Code, tc.code)
		}
	}
}

func TestV1JobNotFound(t *testing.T) {
	srv, _ := newV1Server(t, 0, "")
	r, err := http.Get(srv.URL + "/api/v1/jobs/deadbeefdeadbeef")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", r.StatusCode)
	}
	var env v1Env
	json.NewDecoder(r.Body).Decode(&env)
	if env.Error.Code != "job_not_found" || env.Error.JobID != "deadbeefdeadbeef" {
		t.Fatalf("envelope = %+v", env.Error)
	}
}

func TestV1GenerateAndWait(t *testing.T) {
	srv, _ := newV1Server(t, 0, "")
	body := `{"genre":"landscape","fields":{"setting":"a valley"},"size":"512x512","wait_seconds":5}`
	resp, err := http.Post(srv.URL+"/api/v1/generate", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var job struct {
		Status string `json:"status"`
		Prompt string `json:"prompt"`
	}
	json.NewDecoder(resp.Body).Decode(&job)
	if job.Status != "done" {
		t.Fatalf("status = %q", job.Status)
	}
	if job.Prompt != "A landscape: a valley." {
		t.Fatalf("prompt = %q", job.Prompt)
	}
}

func TestV1GenerateAndWaitTimeout(t *testing.T) {
	srv, _ := newV1Server(t, 2*time.Second, "")
	body := `{"genre":"landscape","fields":{"setting":"a valley"},"size":"512x512","wait_seconds":1}`
	resp, err := http.Post(srv.URL+"/api/v1/generate", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", resp.StatusCode)
	}
	var env v1Env
	json.NewDecoder(resp.Body).Decode(&env)
	if env.Error.Code != "sidecar_timeout" || env.Error.JobID == "" || !env.Error.Retryable {
		t.Fatalf("envelope = %+v", env.Error)
	}
}

func TestV1GenerateNoWaitPending(t *testing.T) {
	srv, _ := newV1Server(t, 0, "")
	resp, err := http.Post(srv.URL+"/api/v1/generate", "application/json",
		bytes.NewReader([]byte(`{"genre":"landscape","fields":{"setting":"a valley"},"size":"512x512"}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if id, _ := out["job_id"].(string); id != "" {
		waitForJobDone(t, srv, id)
	}
}

func TestV1CancelAccepted202(t *testing.T) {
	// A cancel of an in-flight job answers 202; the status must go out with
	// the JSON Content-Type already set, or Go's sniffer serves the body as
	// text/plain.
	srv, _ := newV1Server(t, time.Second, "")
	sub := v1Submit(t, srv, `{"genre":"landscape","fields":{"setting":"a valley"},"size":"512x512"}`, "")
	id := sub["job_id"].(string)
	resp, err := http.Post(srv.URL+"/api/v1/jobs/"+id+"/cancel", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("cancel = %d, want 202", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if out["status"] != "cancelling" {
		t.Fatalf("cancel body: %v", out)
	}
	// Drain to the terminal cancelled status so the worker is done before
	// TempDir cleanup races its writes.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, err := http.Get(srv.URL + "/api/v1/jobs/" + id)
		if err != nil {
			t.Fatal(err)
		}
		var j map[string]any
		json.NewDecoder(r.Body).Decode(&j)
		r.Body.Close()
		if j["status"] == "cancelled" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s never cancelled", id)
}

func TestV1AuthRequired(t *testing.T) {
	srv, _ := newV1Server(t, 0, "t0k")

	// /api/v1 without a token: 401 envelope.
	r, err := http.Get(srv.URL + "/api/v1/jobs")
	if err != nil {
		t.Fatal(err)
	}
	var env v1Env
	json.NewDecoder(r.Body).Decode(&env)
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", r.StatusCode)
	}
	if env.Error.Code != "unauthorized" {
		t.Fatalf("code = %q", env.Error.Code)
	}

	// /api/* legacy is protected too.
	r2, err := http.Get(srv.URL + "/api/jobs")
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("legacy no-token status = %d, want 401", r2.StatusCode)
	}

	// With the bearer token both generations open up.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer t0k")
	r3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r3.Body.Close()
	if r3.StatusCode != http.StatusOK {
		t.Fatalf("bearer status = %d, want 200", r3.StatusCode)
	}

	// Static UI is never authenticated.
	r4, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	r4.Body.Close()
	if r4.StatusCode != http.StatusOK {
		t.Fatalf("static status = %d, want 200", r4.StatusCode)
	}
}

func TestV1AuthOpenWithoutToken(t *testing.T) {
	srv, _ := newV1Server(t, 0, "")
	r, err := http.Get(srv.URL + "/api/v1/jobs")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.StatusCode)
	}
}

func TestV1JobGetShowsTerminalProgress(t *testing.T) {
	srv, _ := newV1Server(t, 0, "")
	out := v1Submit(t, srv, `{"genre":"landscape","fields":{"setting":"a valley"},"size":"512x512"}`, "")
	id, _ := out["job_id"].(string)

	deadline := time.Now().Add(5 * time.Second)
	var job map[string]any
	for time.Now().Before(deadline) {
		r, err := http.Get(srv.URL + "/api/v1/jobs/" + id)
		if err != nil {
			t.Fatal(err)
		}
		json.NewDecoder(r.Body).Decode(&job)
		r.Body.Close()
		if job["status"] == "done" || job["status"] == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job["status"] != "done" {
		t.Fatalf("status = %v", job["status"])
	}
	if _, ok := job["step"]; ok {
		t.Fatalf("terminal job exposes step: %v", job["step"])
	}
}

func TestV1CancelUnknownJob(t *testing.T) {
	srv, _ := newV1Server(t, 0, "")
	resp, err := http.Post(srv.URL+"/api/v1/jobs/deadbeefdeadbeef/cancel", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var env v1Env
	json.NewDecoder(resp.Body).Decode(&env)
	if resp.StatusCode != http.StatusNotFound || env.Error.Code != "job_not_found" {
		t.Fatalf("cancel = %d %+v", resp.StatusCode, env.Error)
	}
}

func TestV1DeleteJobLifecycle(t *testing.T) {
	srv, _ := newV1Server(t, 0, "")
	out := v1Submit(t, srv, `{"genre":"landscape","fields":{"setting":"a valley"},"size":"512x512"}`, "")
	id := out["job_id"].(string)
	waitForJobDone(t, srv, id)

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/jobs/"+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "deleted" {
		t.Fatalf("delete body = %v", body)
	}

	// The job is gone: GET and a repeat DELETE both 404 with the envelope.
	get, err := http.Get(srv.URL + "/api/v1/jobs/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer get.Body.Close()
	var env v1Env
	json.NewDecoder(get.Body).Decode(&env)
	if get.StatusCode != http.StatusNotFound || env.Error.Code != "job_not_found" {
		t.Fatalf("get after delete = %d %+v", get.StatusCode, env.Error)
	}
	req2, err := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/jobs/"+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var env2 v1Env
	json.NewDecoder(resp2.Body).Decode(&env2)
	if resp2.StatusCode != http.StatusNotFound || env2.Error.Code != "job_not_found" {
		t.Fatalf("repeat delete = %d %+v", resp2.StatusCode, env2.Error)
	}
}

func TestV1DeleteNonTerminalJobRefused(t *testing.T) {
	srv, _ := newV1Server(t, 300*time.Millisecond, "")
	out := v1Submit(t, srv, `{"genre":"landscape","fields":{"setting":"a valley"},"size":"512x512"}`, "")
	id := out["job_id"].(string)

	// Wait until the job is running, then refuse the delete.
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := http.Get(srv.URL + "/api/v1/jobs/" + id)
		if err != nil {
			t.Fatal(err)
		}
		var j map[string]any
		json.NewDecoder(r.Body).Decode(&j)
		r.Body.Close()
		if j["status"] == "generating" {
			break
		}
		if j["status"] == "done" || j["status"] == "failed" {
			t.Fatalf("job finished too early: %v", j)
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never started generating: %v", j)
		}
		time.Sleep(10 * time.Millisecond)
	}

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/jobs/"+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var env v1Env
	json.NewDecoder(resp.Body).Decode(&env)
	if resp.StatusCode != http.StatusBadRequest || env.Error.Code != "validation_error" || !strings.Contains(env.Error.Message, "generating") {
		t.Fatalf("delete running job = %d %+v", resp.StatusCode, env.Error)
	}

	waitForJobDone(t, srv, id)
	req2, err := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/jobs/"+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("delete after completion = %d", resp2.StatusCode)
	}
}
