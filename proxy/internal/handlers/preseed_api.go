package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/Vigno04/jellyproxy/proxy/internal/auth"
	"github.com/Vigno04/jellyproxy/proxy/internal/config"
	"github.com/Vigno04/jellyproxy/proxy/internal/preseed"
)

// PreseedAPI exposes pre-seeding jobs to the companion plugin.
type PreseedAPI struct {
	manager *preseed.Manager
	secret  string
}

// NewPreseedAPI wires the API to a manager.
func NewPreseedAPI(cfg *config.Config, m *preseed.Manager) *PreseedAPI {
	return &PreseedAPI{manager: m, secret: cfg.SharedSecret}
}

// Start handles POST /jellyproxy/api/preseed/start.
func (a *PreseedAPI) Start(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var v struct {
		ID       string      `json:"id"`
		ItemName string      `json:"item_name"`
		URLs     []string    `json:"urls"`
		Headers  http.Header `json:"headers"`
	}
	if json.NewDecoder(r.Body).Decode(&v) != nil || len(v.URLs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "urls are required"})
		return
	}
	if err := a.manager.StartWithOptions(v.ID, v.ItemName, v.URLs, v.Headers); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started", "id": v.ID})
}

// Jobs handles GET /jellyproxy/api/preseed/jobs.
func (a *PreseedAPI) Jobs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.manager.Jobs())
}

// Cancel handles POST /jellyproxy/api/preseed/cancel.
func (a *PreseedAPI) Cancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var v struct {
		ID string `json:"id"`
	}
	if json.NewDecoder(r.Body).Decode(&v) != nil || !a.manager.Cancel(v.ID) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "job not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// Handler wraps fn with shared-secret authentication.
func (a *PreseedAPI) Handler(fn http.HandlerFunc) http.HandlerFunc {
	return auth.RequireSecret(a.secret, fn)
}
