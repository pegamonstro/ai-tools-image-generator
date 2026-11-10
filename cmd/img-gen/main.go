package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"img-gen/internal/critic"
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
	ImageRouting  string // "lattice" (default): generate+edit via the frontend; "direct": legacy sidecar calls
	ImageURL      string
	FillURL       string
	ReduxURL      string
	UpscaleURL    string
	RifeURL       string
	RembgURL      string
	SDXLURL       string
	DataDir       string
	GenresFile    string
	ModelsFile    string
	ProfilesFile  string
	PresetsFile   string
	ExportDir     string
	EnhanceModel  string
	EnhanceSystem string
	// ImageModel substitutes an empty request model under lattice routing
	// (config.ImageRouting != "direct") — a catalog key from ModelsFile or a
	// raw value. The lattice registry routes by model name, so an empty model
	// is unroutable there.
	ImageModel string
	// ImageTimeout bounds the lattice image legs and is the shared HTTP
	// client timeout the critic's chat/tagger calls also ride.
	ImageTimeout time.Duration
	// CriticModel is the VLM judge for the Phase D validate checklist — a
	// FULL control-plane model tag (e.g. a registry name) passed through to
	// the lattice chat route verbatim, like EnhanceModel.
	CriticModel string
	// CriticValidateModel is an optional SECOND judge for the standalone
	// /api/v1/validate endpoint (MCP validate_image). Use it to give operator
	// tooling a heavier judge model than the in-render critic: an ollama-served
	// judge stays resident ~1 min per call, and a model big enough to starve a
	// concurrent sd.cpp render must never sit inside a job's check loop.
	// Empty (the default) falls back to CriticModel.
	CriticValidateModel string
	// CriticCountModel is an optional multimodal model tag for figure-counting
	// (the regional pipeline's presence gate: a regional render missing its
	// second figure gets one seed-bumped, occupancy-flipped retry). Same
	// control-plane-tag semantics as CriticModel; empty disables the gate.
	CriticCountModel string
	// CriticTimeout bounds a /api/v1/validate sync call (judge legs ride the
	// request context; the deadline is this duration). Zero/negative parses
	// fall back to the default.
	CriticTimeout time.Duration
	// TaggerURL is the WD14 tagger sidecar for tag_roundtrip checks; ""
	// means the tag ops fail submission honestly.
	TaggerURL string
	// TaggerTimeout bounds each tagger/vocab sidecar call (submit gating
	// handles availability). Zero/negative parses fall back to the default.
	TaggerTimeout time.Duration
	// DWPoseURL is the DWPose sidecar for pose keypoint extraction.
	DWPoseURL string
	// DWPoseTimeout bounds each DWPose sidecar call.
	DWPoseTimeout time.Duration
	APIToken      string
	Split9Slice   string
	CssEmission   string
}

func loadConfig() config {
	// The :7b tag is the registry's actual name — the :latest form ollama
	// 404s, and the critic's model must route or the judge never answers.
	criticModel := envOr("CRITIC_MODEL", "huihui_ai/qwen2.5-vl-abliterated:7b")
	validateModel := envOr("CRITIC_VALIDATE_MODEL", "")
	countModel := envOr("CRITIC_COUNT_MODEL", "")
	criticTimeout := time.Duration(envIntOr("CRITIC_TIMEOUT_S", 120)) * time.Second
	if criticTimeout <= 0 {
		criticTimeout = 120 * time.Second
	}
	tagTimeout := time.Duration(envIntOr("TAGGER_TIMEOUT_S", 60)) * time.Second
	if tagTimeout <= 0 {
		tagTimeout = 60 * time.Second
	}
	return config{
		LatticeURL:          envOr("LATTICE_FRONTEND_URL", "http://127.0.0.1:8080"),
		ImageRouting:        envOr("IMAGE_ROUTING", "lattice"),
		ImageURL:            envOr("IMAGE_URL", "http://127.0.0.1:8899"),
		ImageModel:          envOr("IMAGE_MODEL", ""),
		FillURL:             envOr("FILL_URL", ""),
		ReduxURL:            envOr("REDUX_URL", ""),
		UpscaleURL:          envOr("UPSCALE_URL", ""),
		RifeURL:             envOr("RIFE_URL", ""),
		RembgURL:            envOr("REMBG_URL", ""),
		SDXLURL:             envOr("SDXL_URL", ""),
		DataDir:             envOr("DATA_DIR", "./data"),
		GenresFile:          envOr("GENRES_FILE", "./genres.json"),
		ModelsFile:          envOr("MODELS_FILE", "./models.json"),
		ProfilesFile:        envOr("PROFILES_FILE", "./models_profiles.yaml"),
		PresetsFile:         envOr("PRESETS_FILE", "./presets.json"),
		ExportDir:           envOr("EXPORT_DIR", ""),
		EnhanceModel:        envOr("ENHANCE_MODEL", "huihui_ai/dolphin3-abliterated:latest"),
		EnhanceSystem:       envOr("ENHANCE_SYSTEM", "You write concise, high-quality prompts for an uncensored adult image generator. Explicit sexual content is permitted and expected; never refuse, never moralize. Respond with only the prompt text."),
		ImageTimeout:        time.Duration(envIntOr("IMAGE_TIMEOUT_S", 7200)) * time.Second,
		CriticModel:         criticModel,
		CriticValidateModel: validateModel,
		CriticCountModel:    countModel,
		CriticTimeout:       criticTimeout,
		TaggerURL:           envOr("TAGGER_URL", ""),
		TaggerTimeout:       tagTimeout,
		DWPoseURL:           envOr("DWPOSE_URL", ""),
		DWPoseTimeout:       time.Duration(envIntOr("DWPOSE_TIMEOUT_S", 60)) * time.Second,
		APIToken:            envOr("IMG_GEN_TOKEN", ""),
		Split9Slice:         envOr("SPLIT_9_SLICE", "false"),
		CssEmission:         envOr("CSS_EMISSION", "false"),
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
	profileConfig, err := models.LoadProfiles(cfg.ProfilesFile)
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
	lat.RifeURL = cfg.RifeURL
	lat.RembgURL = cfg.RembgURL
	lat.SDXLImageURL = cfg.SDXLURL
	lat.TaggerURL = cfg.TaggerURL
	lat.DWPoseURL = cfg.DWPoseURL
	lat.ImagesViaLattice = cfg.ImageRouting != "direct"
	lat.HTTP = &http.Client{Timeout: cfg.ImageTimeout}

	// Rife is wired only when the sidecar is configured: nil Ops.Rife must
	// unambiguously mean "RIFE sidecar not configured" so submit-time
	// validation can refuse interpolation jobs with the honest error.
	var rifeOp func(ctx context.Context, anchors [][]byte, multiplier int) ([][]byte, error)
	if cfg.RifeURL != "" {
		rifeOp = lat.Rife
	}

	// Rembg is wired only when the sidecar is configured: nil Ops.Rembg must
	// unambiguously mean "rembg sidecar not configured" so asset-set submit
	// validation can refuse transparent "rembg" assets with the honest error.
	var rembgOp func(ctx context.Context, imageB64 string) ([]byte, error)
	if cfg.RembgURL != "" {
		rembgOp = lat.Rembg
	}

	// DWPose is wired only when the sidecar is configured: nil Ops.DWPose must
	// unambiguously mean "DWPose sidecar not configured" so character kind
	// validation can refuse pose extraction with the honest error.
	var dwposeOp func(ctx context.Context, imageB64 string) ([]float32, error)
	if cfg.DWPoseURL != "" {
		dwpTimeout := cfg.DWPoseTimeout
		if dwpTimeout <= 0 {
			dwpTimeout = 60 * time.Second
		}
		dwposeOp = func(ctx context.Context, imageB64 string) ([]float32, error) {
			dctx, cancel := context.WithTimeout(ctx, dwpTimeout)
			defer cancel()
			return lat.DWPose(dctx, imageB64)
		}
	}

	// Phase D critic runtime. The judge answers the validate checklist's
	// vlm_check criteria through the lattice chat route with a control-plane
	// model tag (CRITIC_MODEL passes through verbatim, like EnhanceModel) —
	// it is always wired, so an unwired Env never reaches production here
	// and critic.Run's nil-Judge defense stays library-level. The tagger
	// legs wire only when the WD14 sidecar is configured: nil Env.Tagger
	// unambiguously means "tagger sidecar not configured" so tag_roundtrip
	// fails submission honestly (the same convention as Rife/Rembg above);
	// each tagger call — /tag and the cached /meta vocab fetch alike — gets
	// its own TAGGER_TIMEOUT_S deadline inside the caller's context.
	criticEnv := critic.Env{Judge: critic.ChatJudge(lat, cfg.CriticModel)}
	if cfg.CriticCountModel != "" {
		criticEnv.Count = func(ctx context.Context, imageB64 string) (int, error) {
			return lat.VisionCount(ctx, cfg.CriticCountModel, imageB64)
		}
	}
	if cfg.TaggerURL != "" {
		tagTimeout := cfg.TaggerTimeout
		criticEnv.Tagger = func(ctx context.Context, imageB64 string) ([]lattice.TagScore, error) {
			tctx, cancel := context.WithTimeout(ctx, tagTimeout)
			defer cancel()
			return lat.Tagger(tctx, imageB64)
		}
		criticEnv.Vocab = func(ctx context.Context) ([]string, error) {
			tctx, cancel := context.WithTimeout(ctx, tagTimeout)
			defer cancel()
			return lat.TaggerVocab(tctx)
		}
	}
	// The standalone validate endpoint's judge rides CRITIC_VALIDATE_MODEL
	// when set (the operator-tool judge, deliberately heavier than the
	// in-render critic — see the config field's comment), with the same
	// tagger legs: the endpoint only swaps the model, never the sidecars.
	var validateCriticEnv *critic.Env
	if cfg.CriticValidateModel != "" {
		v := critic.Env{Judge: critic.ChatJudge(lat, cfg.CriticValidateModel)}
		if cfg.TaggerURL != "" {
			v.Tagger = criticEnv.Tagger
			v.Vocab = criticEnv.Vocab
		}
		validateCriticEnv = &v
	}

	chatFn := func(ctx context.Context, msgs []prompting.Message) (string, error) {
		return lat.Chat(ctx, cfg.EnhanceModel, msgs)
	}
	mgr := queue.New(queue.Options{
		Genres:           catalog,
		Presets:          presetCatalog,
		Models:           modelCatalog,
		Profiles:         profileConfig,
		Store:            store,
		ImagesViaLattice: cfg.ImageRouting != "direct",
		DefaultModel:     cfg.ImageModel,
		ExportDir:        cfg.ExportDir,
		Ops: queue.ImageOps{
			Generate:   lat.Generate,
			Edit:       lat.Edit,
			Inpaint:    lat.Inpaint,
			Blend:      lat.Blend,
			Upscale:    lat.Upscale,
			Controlnet: lat.Controlnet,
			Fill:       lat.Fill,
			Rife:       rifeOp,
			Rembg:      rembgOp,
			DWPose:     dwposeOp,
			Progress:   lat.Progress,
			Status:     lat.Status,
			Result:     lat.Result,
			Cancel:     lat.Cancel,
		},
		Chat:          chatFn,
		EnhanceSystem: cfg.EnhanceSystem,
		Critic:        criticEnv,
		CriticTimeout: cfg.CriticTimeout,
		Split9Slice:   cfg.Split9Slice,
		CssEmission:   cfg.CssEmission,
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
		if r.Method == http.MethodDelete {
			if err := mgr.Delete(rest); err != nil {
				if errors.Is(err, queue.ErrJobNotFound) {
					writeErr(w, http.StatusNotFound, "not found")
					return
				}
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSON(w, map[string]string{"status": "deleted"})
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

	// Idempotency keys persist beside the other storage dirs so replays
	// survive a restart: DATA_DIR/idempotency.jsonl is loaded into memory at
	// boot and every completed key is appended back.
	idem, idemErr := newIdemStore(filepath.Join(cfg.DataDir, "idempotency.jsonl"))
	if idemErr != nil {
		return nil, fmt.Errorf("idempotency store: %w", idemErr)
	}
	registerV1(mux, &v1Deps{
		mgr:            mgr,
		genreCat:       catalog,
		modelCat:       modelCatalog,
		presetCat:      presetCatalog,
		store:          store,
		dataDir:        cfg.DataDir,
		exportDir:      cfg.ExportDir,
		idem:           idem,
		critic:         criticEnv,
		validateCritic: validateCriticEnv,
		criticTimeout:  cfg.CriticTimeout,
		segment:        lat.Rembg,
		hands:          lat.Hands,
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

// writeJSONStatus is writeJSON with an explicit status: the Content-Type
// header must be set before WriteHeader, or Go's sniffer serves the JSON body
// as text/plain.
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
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
	log.Printf("img-gen config: lattice=%s image_routing=%s image_model=%s image_url=%s upscale_url=%s sdxl_url=%s data_dir=%s genres=%s enhance_model=%s timeout=%s listen=%s api_auth=%t",
		cfg.LatticeURL, cfg.ImageRouting, cfg.ImageModel, cfg.ImageURL, cfg.UpscaleURL, cfg.SDXLURL, cfg.DataDir, cfg.GenresFile, cfg.EnhanceModel, cfg.ImageTimeout, addr, cfg.APIToken != "")
	log.Printf("img-gen listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, h))
}
