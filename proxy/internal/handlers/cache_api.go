package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jellyproxy-project/jellyproxy/proxy/internal/cache"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/config"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/auth"
)

// CacheAPIHandler exposes storage telemetry, runtime limit configuration, purge, and invalidation endpoints
type CacheAPIHandler struct {
	cfg     *config.Config
	storage *cache.StorageManager
}

// NewCacheAPIHandler creates a handler for cache and storage management endpoints
func NewCacheAPIHandler(cfg *config.Config, storage *cache.StorageManager) *CacheAPIHandler {
	return &CacheAPIHandler{
		cfg:     cfg,
		storage: storage,
	}
}

// ConfigUpdateRequest payload for POST /jellyproxy/api/config
type ConfigUpdateRequest struct {
	Secret       string  `json:"secret,omitempty"`
	MaxStorageGB float64 `json:"max_storage_gb"`
}

// PurgeRequest payload for POST /jellyproxy/api/cache/purge
type PurgeRequest struct {
	Secret   string `json:"secret,omitempty"`
	Category string `json:"category"`
}

// InvalidateRequest payload for POST /jellyproxy/api/cache/invalidate
type InvalidateRequest struct {
	Secret string `json:"secret,omitempty"`
	ItemID string `json:"item_id"`
	Type   string `json:"type,omitempty"`
}

// authorize checks the shared secret against X-JellyProxy-Secret header, Authorization Bearer token, query param, or JSON body secret
func (h *CacheAPIHandler) authorize(w http.ResponseWriter, r *http.Request, bodySecret string) bool {
	if bodySecret != "" && auth.IsAuthorized(r, h.cfg.SharedSecret) { return true }
	if h.cfg.SharedSecret == "" {
		return true
	}

	secret := r.Header.Get("X-JellyProxy-Secret")
	if secret == "" {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			secret = strings.TrimSpace(auth[7:])
		}
	}
	if secret == "" {
		secret = r.URL.Query().Get("secret")
	}
	if secret == "" {
		secret = bodySecret
	}

	if secret != h.cfg.SharedSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"status":  "unauthorized",
			"message": "Invalid or missing shared secret",
		})
		return false
	}
	return true
}

// HandleStorage handles GET /jellyproxy/api/storage
func (h *CacheAPIHandler) HandleStorage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	if !h.authorize(w, r, "") {
		return
	}

	stats := h.storage.GetStats()
	writeJSON(w, http.StatusOK, stats)
}

// HandleConfig handles POST /jellyproxy/api/config
func (h *CacheAPIHandler) HandleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ConfigUpdateRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"status":  "error",
				"message": "Invalid JSON body",
			})
			return
		}
	}

	if !h.authorize(w, r, req.Secret) {
		return
	}

	if req.MaxStorageGB <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"status":  "error",
			"message": "max_storage_gb must be greater than 0",
		})
		return
	}

	if err := h.storage.SetMaxStorageGB(req.MaxStorageGB); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"status":  "error",
			"message": err.Error(),
		})
		return
	}

	h.cfg.MaxStorageGB = req.MaxStorageGB
	stats := h.storage.GetStats()

	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"message":        "Storage configuration updated",
		"max_storage_gb": req.MaxStorageGB,
		"max_bytes":      stats.MaxBytes,
		"total_bytes":    stats.TotalBytes,
		"storage":        stats,
	})
}

// HandlePurge handles POST /jellyproxy/api/cache/purge
func (h *CacheAPIHandler) HandlePurge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req PurgeRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"status":  "error",
				"message": "Invalid JSON body",
			})
			return
		}
	}

	if !h.authorize(w, r, req.Secret) {
		return
	}

	if req.Category == "" {
		req.Category = cache.CategoryAll
	}

	purgedCount, err := h.storage.Purge(req.Category)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"status":  "error",
			"message": err.Error(),
		})
		return
	}

	stats := h.storage.GetStats()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":       "ok",
		"category":     req.Category,
		"purged_files": purgedCount,
		"storage":      stats,
	})
}

// HandleInvalidate handles POST /jellyproxy/api/cache/invalidate
func (h *CacheAPIHandler) HandleInvalidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req InvalidateRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"status":  "error",
				"message": "Invalid JSON body",
			})
			return
		}
	}

	if !h.authorize(w, r, req.Secret) {
		return
	}

	if strings.TrimSpace(req.ItemID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"status":  "error",
			"message": "item_id is required",
		})
		return
	}

	removedCount := h.storage.InvalidateByItemID(req.ItemID, req.Type)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":            "ok",
		"item_id":           req.ItemID,
		"type":              req.Type,
		"invalidated_files": removedCount,
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}