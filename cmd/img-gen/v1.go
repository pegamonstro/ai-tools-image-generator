package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"img-gen/internal/genres"
	"img-gen/internal/models"
	"img-gen/internal/presets"
	"img-gen/internal/queue"
	"img-gen/internal/storage"
)

// v1 is the versioned agent-facing API surface. Legacy /api/* handlers in
// main.go keep byte-identical behavior; all contract changes land here.

type v1Deps struct {
	mgr       *queue.Manager
	genreCat  *genres.Catalog
	modelCat  *models.Catalog
	presetCat *presets.Catalog
	store     *storage.Store
	dataDir   string
	exportDir string
	idem      idemStore
}

func registerV1(mux *http.ServeMux, d *v1Deps) {
	mux.HandleFunc("/api/v1/catalogs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"genres":  d.genreCat,
			"models":  d.modelCat,
			"presets": d.presetCat,
		})
	})

	mux.HandleFunc("/api/v1/genres", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, d.genreCat)
	})

	mux.HandleFunc("/api/v1/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, d.modelCat)
	})

	mux.HandleFunc("/api/v1/presets", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, d.presetCat)
	})

	mux.HandleFunc("/api/v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			d.handleV1Submit(w, r)
		case http.MethodGet:
			writeJSON(w, d.mgr.List())
		default:
			writeV1Err(w, http.StatusMethodNotAllowed, "validation_error", "method not allowed", "")
		}
	})

	mux.HandleFunc("/api/v1/jobs/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/jobs/")
		if rest == "" {
			writeV1Err(w, http.StatusNotFound, "job_not_found", "job not found", "")
			return
		}
		if strings.HasSuffix(rest, "/events") {
			handleV1SSE(w, r, d.mgr, strings.TrimSuffix(rest, "/events"))
			return
		}
		if strings.HasSuffix(rest, "/cancel") {
			d.handleV1Cancel(w, r, strings.TrimSuffix(rest, "/cancel"))
			return
		}
		j, ok := d.mgr.Get(rest)
		if !ok {
			writeV1Err(w, http.StatusNotFound, "job_not_found", "job not found", rest)
			return
		}
		writeJSON(w, j)
	})

	mux.HandleFunc("/api/v1/generate", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeV1Err(w, http.StatusMethodNotAllowed, "validation_error", "method not allowed", "")
			return
		}
		d.handleV1Generate(w, r)
	})

	mux.HandleFunc("/api/v1/images/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/api/v1/images/")
		serveStoredImage(w, r, d.dataDir, name, v1ErrWriter())
	})

	mux.HandleFunc("/api/v1/export", func(w http.ResponseWriter, r *http.Request) {
		d.handleV1Export(w, r, d.exportDir)
	})
}

// v1SubmitResponse is the shared POST-body shape: {"job_id"} or {"job_ids"}.
func (d *v1Deps) handleV1Submit(w http.ResponseWriter, r *http.Request) {
	var req queue.SubmitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeV1Err(w, http.StatusBadRequest, "validation_error", "bad body", "")
		return
	}

	key := r.Header.Get("Idempotency-Key")
	if key != "" {
		switch state, reply := d.idem.reserve(key); state {
		case "replay":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(reply.status)
			_, _ = w.Write(reply.raw)
			return
		case "conflict":
			writeV1Err(w, http.StatusConflict, "bad_idempotency_key", "idempotency key already in flight", "")
			return
		}
	}

	var body []byte
	if req.Batch > 1 {
		ids, err := d.mgr.SubmitBatch(req)
		if err != nil {
			d.idem.release(key)
			v1SubmitErr(w, err)
			return
		}
		body, _ = json.Marshal(map[string][]string{"job_ids": ids})
	} else {
		id, err := d.mgr.Submit(req)
		if err != nil {
			d.idem.release(key)
			v1SubmitErr(w, err)
			return
		}
		body, _ = json.Marshal(map[string]string{"job_id": id})
	}
	if key != "" {
		d.idem.complete(key, http.StatusCreated, body)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(body)
}

func v1SubmitErr(w http.ResponseWriter, err error) {
	var ve *queue.ValidationError
	if errors.As(err, &ve) {
		writeV1Err(w, http.StatusBadRequest, ve.Code, ve.Msg, "")
		return
	}
	writeV1Err(w, http.StatusInternalServerError, "generation_failed", err.Error(), "")
}

type generateAndWaitRequest struct {
	queue.SubmitRequest
	WaitSeconds int `json:"wait_seconds"`
}

// handleV1Generate submits a job and optionally blocks on it. wait_seconds > 0
// waits for a terminal status (final Job JSON, 200) or times out with 504.
func (d *v1Deps) handleV1Generate(w http.ResponseWriter, r *http.Request) {
	var req generateAndWaitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeV1Err(w, http.StatusBadRequest, "validation_error", "bad body", "")
		return
	}
	if req.Batch > 1 && req.WaitSeconds > 0 {
		writeV1Err(w, http.StatusBadRequest, "validation_error", "batch is not supported with wait_seconds > 0", "")
		return
	}

	var id string
	if req.Batch > 1 {
		ids, err := d.mgr.SubmitBatch(req.SubmitRequest)
		if err != nil {
			v1SubmitErr(w, err)
			return
		}
		id = ids[0]
	} else {
		var err error
		id, err = d.mgr.Submit(req.SubmitRequest)
		if err != nil {
			v1SubmitErr(w, err)
			return
		}
	}

	if req.WaitSeconds <= 0 {
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]string{"job_id": id})
		return
	}
	j, done := d.mgr.Wait(id, time.Duration(req.WaitSeconds)*time.Second)
	if !done {
		writeV1Err(w, http.StatusGatewayTimeout, "sidecar_timeout", "not terminal within wait_seconds", id)
		return
	}
	writeJSON(w, j)
}

func (d *v1Deps) handleV1Cancel(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeV1Err(w, http.StatusMethodNotAllowed, "validation_error", "method not allowed", id)
		return
	}
	if _, ok := d.mgr.Get(id); !ok {
		writeV1Err(w, http.StatusNotFound, "job_not_found", "job not found", id)
		return
	}
	if err := d.mgr.Cancel(id); err != nil {
		writeV1Err(w, http.StatusBadRequest, "validation_error", err.Error(), id)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]string{"status": "cancelling"})
}

func handleV1SSE(w http.ResponseWriter, r *http.Request, mgr *queue.Manager, id string) {
	if id == "" {
		writeV1Err(w, http.StatusNotFound, "job_not_found", "job not found", "")
		return
	}
	if _, ok := mgr.Get(id); !ok {
		writeV1Err(w, http.StatusNotFound, "job_not_found", "job not found", id)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeV1Err(w, http.StatusInternalServerError, "generation_failed", "streaming unsupported", id)
		return
	}
	ch, cancel := mgr.Subscribe(id)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flushSSE(w, r, fl, ch)
}

// flushSSE is the shared SSE loop used by both the legacy and v1 handlers.
func flushSSE(w http.ResponseWriter, r *http.Request, fl http.Flusher, ch <-chan queue.Event) {
	for {
		select {
		case ev := <-ch:
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
			if ev.Status == "done" || ev.Status == "failed" {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

// withAPIAuth enforces Bearer auth on /api/* when a token is configured.
// Static UI paths are never authenticated.
func withAPIAuth(next http.Handler, token string) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			const prefix = "Bearer "
			h := r.Header.Get("Authorization")
			if !strings.HasPrefix(h, prefix) || strings.TrimSpace(h[len(prefix):]) != token {
				writeV1Err(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token", "")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func v1ErrWriter() func(http.ResponseWriter, int, string) {
	return func(w http.ResponseWriter, status int, msg string) {
		code := "validation_error"
		switch {
		case strings.Contains(msg, "not found"):
			code = "job_not_found"
		}
		writeV1Err(w, status, code, msg, "")
	}
}

func (d *v1Deps) handleV1Export(w http.ResponseWriter, r *http.Request, exportDir string) {
	if r.Method != http.MethodPost {
		writeV1Err(w, http.StatusMethodNotAllowed, "validation_error", "method not allowed", "")
		return
	}
	var req struct {
		IDs  []string `json:"ids"`
		Dest string   `json:"dest"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeV1Err(w, http.StatusBadRequest, "validation_error", "bad body", "")
		return
	}
	dest := req.Dest
	if dest == "" {
		dest = exportDir
	}
	if dest == "" {
		home, _ := os.UserHomeDir()
		dest = filepath.Join(home, "Downloads", "img-gen")
	}
	if !filepath.IsAbs(dest) {
		writeV1Err(w, http.StatusBadRequest, "validation_error", "dest must be an absolute path", "")
		return
	}
	var copied, skipped []string
	for _, id := range req.IDs {
		dst, err := d.store.CopyImage(id, dest)
		if err != nil {
			skipped = append(skipped, id)
			continue
		}
		copied = append(copied, dst)
	}
	writeJSON(w, map[string]any{"copied": copied, "skipped": skipped})
}

// serveStoredImage is the shared image-file handler (legacy + v1). errf styles
// the error surface: plain text for legacy, envelope for v1.
func serveStoredImage(w http.ResponseWriter, r *http.Request, dataDir, name string, errf func(http.ResponseWriter, int, string)) {
	if !strings.HasSuffix(name, ".png") {
		errf(w, http.StatusNotFound, "not found")
		return
	}
	id := strings.TrimSuffix(name, ".png")
	if !storage.ValidImageID(id) {
		errf(w, http.StatusNotFound, "not found")
		return
	}
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", id+".png"))
	}
	http.ServeFile(w, r, filepath.Join(dataDir, "images", id+".png"))
}

// idemStore remembers job-submit responses by Idempotency-Key. Entries are
// in-memory only and are cleared on restart; a replayed key can only return
// the response captured in this process's lifetime.
type idemStore struct {
	mu   sync.Mutex
	seen map[string]*idemEntry
}

type idemEntry struct {
	done   bool
	status int
	body   []byte
}

// reserve classifies a key as "replay" (return the recorded response),
// "conflict" (a submit for this key is still in flight), or "fresh" (key
// reserved from now on).
func (s *idemStore) reserve(key string) (state string, reply envelopeBody) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]*idemEntry{}
	}
	if e, ok := s.seen[key]; ok {
		if e.done {
			return "replay", envelopeBody{status: e.status, raw: e.body}
		}
		return "conflict", envelopeBody{}
	}
	s.seen[key] = &idemEntry{}
	return "fresh", envelopeBody{}
}

func (s *idemStore) complete(key string, status int, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen[key] = &idemEntry{done: true, status: status, body: body}
}

// release drops an in-flight reservation so a failed submit can be retried
// under the same key.
func (s *idemStore) release(key string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.seen, key)
}

// ---------------------------------------------------------------- envelope

type envelopeBody struct {
	status int
	raw    []byte
}

// writeV1Err emits the v1 error envelope:
//
//	{"error":{"code":"<machine>","message":"<human>","job_id":"<opt>","retryable":<bool>}}
func writeV1Err(w http.ResponseWriter, status int, code, message, jobID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	type body struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		JobID     string `json:"job_id,omitempty"`
		Retryable bool   `json:"retryable"`
	}
	_ = json.NewEncoder(w).Encode(struct {
		Error body `json:"error"`
	}{body{
		Code:      code,
		Message:   message,
		JobID:     jobID,
		Retryable: retryableCode(code),
	}})
}

func retryableCode(code string) bool {
	switch code {
	case "sidecar_unavailable", "sidecar_busy", "sidecar_timeout":
		return true
	}
	return false
}
