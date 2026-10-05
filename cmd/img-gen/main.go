package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"img-gen/internal/genres"
	"img-gen/internal/lattice"
	"img-gen/internal/prompting"
	"img-gen/internal/queue"
	"img-gen/internal/storage"
)

//go:embed static
var staticFS embed.FS

var idRe = regexp.MustCompile(`^[a-f0-9]{16}$`)

type config struct {
	LatticeURL    string
	ImageURL      string
	FillURL       string
	DataDir       string
	GenresFile    string
	EnhanceModel  string
	EnhanceSystem string
	ImageTimeout  time.Duration
}

func loadConfig() config {
	return config{
		LatticeURL:    envOr("LATTICE_FRONTEND_URL", "http://127.0.0.1:8080"),
		ImageURL:      envOr("IMAGE_URL", "http://127.0.0.1:8899"),
		FillURL:       envOr("FILL_URL", ""),
		DataDir:       envOr("DATA_DIR", "./data"),
		GenresFile:    envOr("GENRES_FILE", "./genres.json"),
		EnhanceModel:  envOr("ENHANCE_MODEL", "local-brain"),
		EnhanceSystem: envOr("ENHANCE_SYSTEM", "You write concise, high-quality image-generation prompts. Respond with only the prompt text."),
		ImageTimeout:  time.Duration(envIntOr("IMAGE_TIMEOUT_S", 7200)) * time.Second,
	}
}

func newHandler(cfg config) (http.Handler, error) {
	catalog, err := genres.Load(cfg.GenresFile)
	if err != nil {
		return nil, err
	}
	store, err := storage.New(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	lat := lattice.New(cfg.LatticeURL)
	lat.ImageURL = cfg.ImageURL
	lat.FillURL = cfg.FillURL
	lat.HTTP = &http.Client{Timeout: cfg.ImageTimeout}

	chatFn := func(ctx context.Context, msgs []prompting.Message) (string, error) {
		return lat.Chat(ctx, cfg.EnhanceModel, msgs)
	}
	mgr := queue.New(queue.Options{
		Genres:        catalog,
		Store:         store,
		Ops: queue.ImageOps{
			Generate: lat.Generate,
			Edit:     lat.Edit,
			Inpaint:  lat.Inpaint,
			Blend:    lat.Blend,
		},
		Chat:          chatFn,
		EnhanceSystem: cfg.EnhanceSystem,
	})

	mux := http.NewServeMux()

	mux.HandleFunc("/api/genres", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, catalog)
	})

	mux.HandleFunc("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var req queue.SubmitRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeErr(w, http.StatusBadRequest, "bad body")
				return
			}
			id, err := mgr.Submit(req)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSON(w, map[string]string{"job_id": id})
		case http.MethodGet:
			writeJSON(w, mgr.List())
		default:
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	})

	mux.HandleFunc("/api/jobs/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/jobs/")
		if strings.HasSuffix(rest, "/events") {
			handleSSE(w, r, mgr, strings.TrimSuffix(rest, "/events"))
			return
		}
		j, ok := mgr.Get(rest)
		if !ok {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		writeJSON(w, j)
	})

	mux.HandleFunc("/api/images/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/api/images/")
		if !strings.HasSuffix(name, ".png") {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		id := strings.TrimSuffix(name, ".png")
		if !idRe.MatchString(id) {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		http.ServeFile(w, r, filepath.Join(cfg.DataDir, "images", id+".png"))
	})

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	return mux, nil
}

func handleSSE(w http.ResponseWriter, r *http.Request, mgr *queue.Manager, id string) {
	if _, ok := mgr.Get(id); !ok {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	ch, cancel := mgr.Subscribe(id)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	http.Error(w, msg, code)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envIntOr(k string, d int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return d
}

func main() {
	cfg := loadConfig()
	h, err := newHandler(cfg)
	if err != nil {
		log.Fatalf("startup: %v", err)
	}
	addr := envOr("LISTEN", ":8099")
	log.Printf("img-gen config: lattice=%s image_url=%s data_dir=%s genres=%s enhance_model=%s timeout=%s listen=%s",
		cfg.LatticeURL, cfg.ImageURL, cfg.DataDir, cfg.GenresFile, cfg.EnhanceModel, cfg.ImageTimeout, addr)
	log.Printf("img-gen listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, h))
}
