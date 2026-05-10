package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/serkostrov/2b-empty-call/internal/config"
	"github.com/serkostrov/2b-empty-call/internal/domain"
	"github.com/serkostrov/2b-empty-call/internal/middleware"
)

type Handler struct {
	cfg    config.Config
	logger *slog.Logger
}

func NewHandler(cfg config.Config, logger *slog.Logger) *Handler {
	return &Handler{cfg: cfg, logger: logger}
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", h.health)
	mux.HandleFunc("GET /readyz", h.health)

	var handler http.Handler = mux
	handler = middleware.AccessLog(h.logger)(handler)
	handler = middleware.Recover(h.logger)(handler)
	handler = middleware.RequestID(handler)
	handler = middleware.CORS(handler)
	return handler
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, domain.HealthResponse{
		Status:      "ok",
		Service:     h.cfg.App.Name,
		Version:     h.cfg.App.Version,
		Environment: h.cfg.App.Env,
		Time:        time.Now().UTC(),
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
