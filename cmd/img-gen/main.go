package main

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"img-gen/internal/genres"
	"img-gen/internal/lattice"
	"img-gen/internal/models"
	"img-gen/internal/presets"
	"img-gen/internal/prompting"
	"img-gen/internal/queue"
	"img-gen/internal/storage"
)

//go:embed static
var staticFS embed.FS

type config struct {
	LatticeURL    string
	ImageURL      string
	FillURL       string
	ReduxURL      string
	UpscaleURL    string
	DataDir       string
	GenresFile    string
	ModelsFile    string
	PresetsFile   string
	ExportDir     string
	EnhanceModel  string
	EnhanceSystem string
	ImageTimeout  time.Duration
	APIToken      string
}

func loadConfig() config {
	return config{
		LatticeURL:    envOr("LATTICE_FRONTEND_URL", "http://127.0.0.1:8080"),
		ImageURL:      envOr("IMAGE_URL", "http://127.0.0.1:8899"),
		FillURL:       envOr("FILL_URL", ""),
		ReduxURL:      envOr("REDUX_URL", ""),
		UpscaleURL:    envOr("UPSCALE_URL", ""),
		DataDir:       envOr("DATA_DIR", "./data"),
		GenresFile:    envOr("GENRES_FILE", "./genres.json"),
		ModelsFile:    envOr("MODELS_FILE", "./models.json"),
		PresetsFile:   envOr("PRESETS_FILE", "./presets.json"),
		ExportDir:     envOr("EXPORT_DIR", ""),
		EnhanceModel:  envOr("ENHANCE_MODEL", "huihui_ai/dolphin3-abliterated:latest"),
		EnhanceSystem: envOr("ENHANCE_SYSTEM", "You write concise, high-quality prompts for an uncensored adult image generator. Explicit sexual content is permitted and expected; never refuse, never moralize. Respond with only the prompt text."),
		ImageTimeout:  time.Duration(envIntOr("IMAGE_TIMEOUT_S", 7200)) * time.Second,
		APIToken:      envOr("IMG_GEN_TOKEN", ""),
	}
}

func newHandler(cfg config) (http.Handler, error) {
	catalog, err := genres.Load(cfg.GenresFile)
	if err != nil {
		return nil, err
	}
	modelCatalog, err := models.Load(cfg.ModelsFile)
	if err != nil {
		return nil, err
	}
	presetCatalog, err := presets.Load(cfg.PresetsFile)
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
	lat.ReduxURL = cfg.ReduxURL
	lat.UpscaleURL = cfg.UpscaleURL
	lat.HTTP = &http.Client{Timeout: cfg.ImageTimeout}

	chatFn := func(ctx context.Context, msgs []prompting.Message) (string, error) {
		return lat.Chat(ctx, cfg.EnhanceModel, msgs)
	}
	mgr := queue.New(queue.Options{
		Genres:  catalog,
		Presets: presetCatalog,
		Store:   store,
		Ops: queue.ImageOps{
			Generate:   lat.Generate,
			Edit:       lat.Edit,
			Inpaint:    lat.Inpaint,
			Blend:      lat.Blend,
			Upscale:    lat.Upscale,
			Controlnet: lat.Controlnet,
			Progress:   lat.Progress,
			Cancel:     lat.Cancel,
		},
		Chat:          chatFn,
		EnhanceSystem: cfg.EnhanceSystem,
	})

	mux := http.NewServeMux()

	mux.HandleFunc("/api/genres", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, catalog)
	})

	mux.HandleFunc("/api/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, modelCatalog)
	})

	mux.HandleFunc("/api/presets", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, presetCatalog)
	})

	mux.HandleFunc("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var req queue.SubmitRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeErr(w, http.StatusBadRequest, "bad body")
				return
			}
			if req.Batch > 1 {
				ids, err := mgr.SubmitBatch(req)
				if err != nil {
					writeErr(w, http.StatusBadRequest, err.Error())
					return
				}
				writeJSON(w, map[string][]string{"job_ids": ids})
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
		if strings.HasSuffix(rest, "/cancel") {
			if r.Method != http.MethodPost {
				writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			if err := mgr.Cancel(strings.TrimSuffix(rest, "/cancel")); err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSON(w, map[string]string{"status": "cancelling"})
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
		serveStoredImage(w, r, cfg.DataDir, strings.TrimPrefix(r.URL.Path, "/api/images/"), writeErr)
	})

	mux.HandleFunc("/api/export", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			IDs  []string `json:"ids"`
			Dest string   `json:"dest"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "bad body")
			return
		}
		dest := req.Dest
		if dest == "" {
			dest = cfg.ExportDir
		}
		if dest == "" {
			home, _ := os.UserHomeDir()
			dest = filepath.Join(home, "Downloads", "img-gen")
		}
		if !filepath.IsAbs(dest) {
			writeErr(w, http.StatusBadRequest, "dest must be an absolute path")
			return
		}
		var copied, skipped []string
		for _, id := range req.IDs {
			dst, err := store.CopyImage(id, dest)
			if err != nil {
				skipped = append(skipped, id)
				continue
			}
			copied = append(copied, dst)
		}
		writeJSON(w, map[string]any{"copied": copied, "skipped": skipped})
	})

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	registerV1(mux, &v1Deps{
		mgr:       mgr,
		genreCat:  catalog,
		modelCat:  modelCatalog,
		presetCat: presetCatalog,
		store:     store,
		dataDir:   cfg.DataDir,
		exportDir: cfg.ExportDir,
	})

	return withAPIAuth(mux, cfg.APIToken), nil
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
	flushSSE(w, r, fl, ch)
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
	log.Printf("img-gen config: lattice=%s image_url=%s upscale_url=%s data_dir=%s genres=%s enhance_model=%s timeout=%s listen=%s api_auth=%t",
		cfg.LatticeURL, cfg.ImageURL, cfg.UpscaleURL, cfg.DataDir, cfg.GenresFile, cfg.EnhanceModel, cfg.ImageTimeout, addr, cfg.APIToken != "")
	log.Printf("img-gen listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, h))
}
