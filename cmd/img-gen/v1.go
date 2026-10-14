package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"img-gen/internal/compose"
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
	idem      *idemStore
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
		if r.Method == http.MethodDelete {
			if err := d.mgr.Delete(rest); err != nil {
				if errors.Is(err, queue.ErrJobNotFound) {
					writeV1Err(w, http.StatusNotFound, "job_not_found", "job not found", rest)
					return
				}
				writeV1Err(w, http.StatusBadRequest, "validation_error", err.Error(), rest)
				return
			}
			writeJSON(w, map[string]string{"status": "deleted"})
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

	mux.HandleFunc("/api/v1/repair", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeV1Err(w, http.StatusMethodNotAllowed, "validation_error", "method not allowed", "")
			return
		}
		var req repairRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&req); err != nil {
			writeV1Err(w, http.StatusBadRequest, "validation_error", "bad body: "+err.Error(), "")
			return
		}
		if req.Image == "" {
			writeV1Err(w, http.StatusBadRequest, "validation_error", "image is required", "")
			return
		}
		if req.Cell != 0 && (req.Cell < compose.RepairMinCell || req.Cell > compose.RepairMaxCell) {
			writeV1Err(w, http.StatusBadRequest, "validation_error",
				fmt.Sprintf("cell must be between %d and %d", compose.RepairMinCell, compose.RepairMaxCell), "")
			return
		}
		colors := req.Colors
		if colors == 0 {
			colors = compose.RepairDefaultColors
		}
		if colors < 1 || colors > compose.RepairMaxColors {
			writeV1Err(w, http.StatusBadRequest, "validation_error",
				fmt.Sprintf("colors must be between 1 and %d", compose.RepairMaxColors), "")
			return
		}
		raw, err := base64.StdEncoding.DecodeString(req.Image)
		if err != nil {
			writeV1Err(w, http.StatusBadRequest, "validation_error", "image is not valid base64", "")
			return
		}
		pre, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			writeV1Err(w, http.StatusBadRequest, "validation_error", "image is not a PNG", "")
			return
		}
		pb := pre.Bounds()
		if pb.Dx() > compose.RepairMaxDim || pb.Dy() > compose.RepairMaxDim {
			writeV1Err(w, http.StatusBadRequest, "validation_error",
				fmt.Sprintf("image is %dx%d; repair supports up to %dx%d", pb.Dx(), pb.Dy(), compose.RepairMaxDim, compose.RepairMaxDim), "")
			return
		}
		out, err := compose.RepairPNG(raw, req.Cell, colors)
		if err != nil {
			writeV1Err(w, http.StatusBadRequest, "validation_error", err.Error(), "")
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	})

	mux.HandleFunc("/api/v1/images/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/api/v1/images/")
		serveStoredImage(w, r, d.dataDir, name, v1ErrWriter())
	})

	mux.HandleFunc("/api/v1/export", func(w http.ResponseWriter, r *http.Request) {
		d.handleV1Export(w, r, d.exportDir)
	})

	mux.HandleFunc("/api/v1/asset-sets", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeV1Err(w, http.StatusMethodNotAllowed, "validation_error", "method not allowed", "")
			return
		}
		d.handleV1AssetSet(w, r)
	})

	mux.HandleFunc("/api/v1/gifs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeV1Err(w, http.StatusMethodNotAllowed, "validation_error", "method not allowed", "")
			return
		}
		d.handleV1GIF(w, r)
	})

	mux.HandleFunc("/api/v1/sets/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/sets/")
		var setID, file string
		if i := strings.Index(rest, "/"); i >= 0 {
			setID, file = rest[:i], rest[i+1:]
		} else {
			setID = rest
		}
		if strings.HasPrefix(file, "files/") && len(file) > len("files/") {
			d.serveSetFile(w, r, setID, strings.TrimPrefix(file, "files/"))
			return
		}
		if file != "" {
			writeV1Err(w, http.StatusNotFound, "job_not_found", "set not found", setID)
			return
		}
		d.serveSet(w, r, setID)
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
		writeJSONStatus(w, http.StatusAccepted, map[string]string{"job_id": id})
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
	writeJSONStatus(w, http.StatusAccepted, map[string]string{"status": "cancelling"})
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

type setAndWaitRequest struct {
	queue.AssetSetRequest
	WaitSeconds int `json:"wait_seconds"`
}

type gifAndWaitRequest struct {
	queue.GifRequest
	WaitSeconds int `json:"wait_seconds"`
}

type repairRequest struct {
	Image  string `json:"image"`
	Cell   int    `json:"cell,omitempty"`
	Colors int    `json:"colors,omitempty"`
}

// handleV1AssetSet submits an asset set. wait_seconds > 0 blocks on the
// parent job; otherwise the caller gets the ids and polls. The set id equals
// the job id, so GET /api/v1/sets/{id} takes either value.
func (d *v1Deps) handleV1AssetSet(w http.ResponseWriter, r *http.Request) {
	var req setAndWaitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeV1Err(w, http.StatusBadRequest, "validation_error", "bad body", "")
		return
	}
	id, err := d.mgr.SubmitAssetSet(req.AssetSetRequest)
	if err != nil {
		v1SubmitErr(w, err)
		return
	}
	waitForCompose(w, d.mgr, id, req.WaitSeconds)
}

func (d *v1Deps) handleV1GIF(w http.ResponseWriter, r *http.Request) {
	var req gifAndWaitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeV1Err(w, http.StatusBadRequest, "validation_error", "bad body", "")
		return
	}
	id, err := d.mgr.SubmitGIF(req.GifRequest)
	if err != nil {
		v1SubmitErr(w, err)
		return
	}
	waitForCompose(w, d.mgr, id, req.WaitSeconds)
}

// waitForCompose is the shared wait branch for the compose endpoints:
// 202 {"job_id","set_id"} when not waiting, the final job JSON otherwise,
// 504 sidecar_timeout when wait_seconds elapses first.
func waitForCompose(w http.ResponseWriter, mgr *queue.Manager, id string, waitSeconds int) {
	if waitSeconds <= 0 {
		writeJSONStatus(w, http.StatusAccepted, map[string]string{"job_id": id, "set_id": id})
		return
	}
	j, done := mgr.Wait(id, time.Duration(waitSeconds)*time.Second)
	if !done {
		writeV1Err(w, http.StatusGatewayTimeout, "sidecar_timeout", "not terminal within wait_seconds", id)
		return
	}
	writeJSON(w, j)
}

// serveSet returns the parent job and its file refs. Anything that is not a
// compose parent reads as "set not found" so plain jobs can't be enumerated
// as sets.
func (d *v1Deps) serveSet(w http.ResponseWriter, r *http.Request, setID string) {
	if !storage.ValidImageID(setID) {
		writeV1Err(w, http.StatusNotFound, "job_not_found", "set not found", setID)
		return
	}
	j, ok := d.mgr.Get(setID)
	if !ok || j.Mode != "compose" {
		writeV1Err(w, http.StatusNotFound, "job_not_found", "set not found", setID)
		return
	}
	writeJSON(w, map[string]any{"set_id": setID, "job": j, "files": j.Files})
}

// setContentType maps a set file's suffix to its serving type; unknown
// suffixes download as octet-stream rather than guessing.
func setContentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".gif"):
		return "image/gif"
	case strings.HasSuffix(name, ".webp"):
		return "image/webp"
	case strings.HasSuffix(name, ".json"):
		return "application/json"
	default:
		return "application/octet-stream"
	}
}

// serveSetFile streams one set file. ReadSetFile enforces the set-file name
// rule and the data-dir boundary, so traversal and malformed names 404 here.
func (d *v1Deps) serveSetFile(w http.ResponseWriter, r *http.Request, setID, name string) {
	if !storage.ValidImageID(setID) {
		writeV1Err(w, http.StatusNotFound, "job_not_found", "set not found", setID)
		return
	}
	j, ok := d.mgr.Get(setID)
	if !ok || j.Mode != "compose" {
		writeV1Err(w, http.StatusNotFound, "job_not_found", "set not found", setID)
		return
	}
	data, err := d.store.ReadSetFile(setID, name)
	if err != nil {
		writeV1Err(w, http.StatusNotFound, "job_not_found", "set file not found", setID)
		return
	}
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	}
	w.Header().Set("Content-Type", setContentType(name))
	_, _ = w.Write(data)
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

// idemStore remembers job-submit responses by Idempotency-Key. With a non-
// empty path the COMPLETED keys are additionally appended to a JSONL file and
// replayed on boot; in-flight entries are deliberately not persisted (a
// restart killed their jobs, so their reservations are dead). With path ==
// "" the store is process-local: the legacy un-persisted behavior.
type idemStore struct {
	mu   sync.Mutex
	seen map[string]*idemEntry
	path string // DATA_DIR/idempotency.jsonl; empty = legacy in-memory behavior (tests)
}

type idemEntry struct {
	done   bool
	status int
	body   []byte
}

// idemRecord is the JSONL persistence form of one COMPLETED key. In-flight
// entries are deliberately not persisted: a restart killed their jobs.
type idemRecord struct {
	Key    string `json:"key"`
	Status int    `json:"status"`
	Body   string `json:"body"` // base64 of the response body
}

const idemReplayLines = 10000 // entries replayed/kept on load
const idemRotateLines = 20000 // above this, the file compacts to idemReplayLines on load

// newIdemStore loads the persisted tail of path (up to idemReplayLines
// COMPLETED records) into memory. A missing file is a fresh store; a torn
// tail line from a crash is tolerated and skipped. When the file has grown
// past idemRotateLines it is compacted back to the replay tail.
func newIdemStore(path string) (*idemStore, error) {
	s := &idemStore{seen: map[string]*idemEntry{}, path: path}
	if path == "" {
		return s, nil
	}
	lines, total, err := readLastLines(path, idemReplayLines)
	if err != nil {
		return nil, err
	}
	for _, ln := range lines {
		var rec idemRecord
		if json.Unmarshal(ln, &rec) != nil || rec.Key == "" {
			continue // tolerate a torn tail line from a crash
		}
		raw, err := base64.StdEncoding.DecodeString(rec.Body)
		if err != nil {
			continue
		}
		s.seen[rec.Key] = &idemEntry{done: true, status: rec.Status, body: raw}
	}
	if total > idemRotateLines {
		if err := writeIdemTail(path, lines); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// readLastLines returns up to n NEWLINE-split lines from the end of path in
// file order, plus the file's total line count. A trailing newline does not
// add an extra line; a torn final line (no trailing newline) still counts as
// a line and is returned as-is so the caller can tolerate it. A missing file
// yields no lines and no error.
func readLastLines(path string, n int) ([][]byte, int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	if len(raw) == 0 {
		return nil, 0, nil
	}
	ring := make([][]byte, n)
	count := 0
	for start := 0; start < len(raw); {
		i := bytes.IndexByte(raw[start:], '\n')
		var ln []byte
		if i < 0 {
			ln = raw[start:] // torn final line without trailing newline
			start = len(raw)
		} else {
			ln = raw[start : start+i]
			start += i + 1
		}
		ring[count%n] = append([]byte(nil), ln...)
		count++
	}
	if count <= n {
		return ring[:count], count, nil
	}
	// file order: ring[count%n:] holds the oldest surviving lines, ring[:count%n] the newest.
	begin := count % n
	out := make([][]byte, 0, n)
	out = append(out, ring[begin:]...)
	out = append(out, ring[:begin]...)
	return out, count, nil
}

// writeIdemTail atomically rewrites path to hold exactly lines (the replay
// tail), one per line: a temp file plus rename, so a crash mid-compaction
// leaves either the old or the new file, never a half-written one.
func writeIdemTail(path string, lines [][]byte) error {
	var b bytes.Buffer
	for _, ln := range lines {
		b.Write(ln)
		b.WriteByte('\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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

// complete marks an in-flight reservation done and, when the store was built
// with a path, appends the COMPLETED record to the JSONL file. In-memory
// semantics stay authoritative: a failed append costs at most one lost
// replay entry after a restart, never a wrong response.
func (s *idemStore) complete(key string, status int, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.seen[key]; ok && !e.done {
		e.done, e.status, e.body = true, status, body
		if s.path != "" {
			rec, _ := json.Marshal(idemRecord{Key: key, Status: status, Body: base64.StdEncoding.EncodeToString(body)})
			f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				log.Printf("idempotency: append open failed for key %q: %v", key, err)
				return
			}
			_, werr := f.Write(append(rec, '\n'))
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			if werr != nil {
				// disk-loss of one replay entry is acceptable (spec); log and keep the in-memory state.
				log.Printf("idempotency: append failed for key %q: %v", key, werr)
			}
		}
	}
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
