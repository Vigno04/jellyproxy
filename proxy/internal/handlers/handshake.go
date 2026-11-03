package handlers

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Vigno04/jellyproxy/proxy/internal/abr"
	"github.com/Vigno04/jellyproxy/proxy/internal/auth"
	"github.com/Vigno04/jellyproxy/proxy/internal/cache"
	"github.com/Vigno04/jellyproxy/proxy/internal/config"
)

// HandshakeManager tracks paired Jellyfin server state and applies initial settings pushed on pair.
type HandshakeManager struct {
	cfg          *config.Config
	abr          *abr.BandwidthManager
	storage      *cache.StorageManager
	mu           sync.RWMutex
	paired       bool
	lastSeen     time.Time
	serverName   string
	serverID     string
	jellyVersion string
}

// HandshakeRequest payload from Jellyfin companion plugin
type HandshakeRequest struct {
	Secret        string      `json:"secret"`
	ServerName    string      `json:"server_name"`
	ServerID      string      `json:"server_id"`
	PluginVersion string      `json:"plugin_version"`
	JellyVersion  string      `json:"jellyfin_version"`
	ABR           *abr.Config `json:"abr,omitempty"`
	Encoders      []string    `json:"encoders,omitempty"`
	MaxStorageGB  float64     `json:"max_storage_gb,omitempty"`
}

// StatusResponse payload sent back to plugin or health check
type StatusResponse struct {
	Status        string    `json:"status"`
	Version       string    `json:"version"`
	UpstreamURL   string    `json:"upstream_url"`
	Paired        bool      `json:"paired"`
	LastSeen      time.Time `json:"last_seen,omitempty"`
	ServerName    string    `json:"server_name,omitempty"`
	ServerID      string    `json:"server_id,omitempty"`
	SecretSecured bool      `json:"secret_secured"`
}

// StatusSnapshot returns the pairing state as a plain map for the dashboard.
func (h *HandshakeManager) StatusSnapshot() map[string]any {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return map[string]any{
		"paired":         h.paired,
		"server_name":    h.serverName,
		"server_id":      h.serverID,
		"jelly_version":  h.jellyVersion,
		"last_seen":      h.lastSeen,
		"upstream_url":   h.cfg.UpstreamURL,
		"secret_secured": h.cfg.SharedSecret != "",
	}
}

// NewHandshakeManager initializes handshake tracker
func NewHandshakeManager(cfg *config.Config) *HandshakeManager {
	return &HandshakeManager{
		cfg: cfg,
	}
}

// SetManagers attaches the bandwidth shaper and cache storage manager so a plugin
// handshake can push its saved settings in a single round-trip.
func (h *HandshakeManager) SetManagers(abrMgr *abr.BandwidthManager, storageMgr *cache.StorageManager) {
	h.mu.Lock()
	h.abr = abrMgr
	h.storage = storageMgr
	h.mu.Unlock()
}

// HandleStatus returns JSON status of the proxy
func (h *HandshakeManager) HandleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	authenticated := auth.IsAuthorized(r, h.cfg.SharedSecret)
	h.mu.RLock()
	resp := StatusResponse{Status: "ok", Version: "0.1.0", Paired: h.paired, SecretSecured: h.cfg.SharedSecret != ""}
	if authenticated {
		resp.UpstreamURL, resp.LastSeen, resp.ServerName, resp.ServerID = h.cfg.UpstreamURL, h.lastSeen, h.serverName, h.serverID
	}
	h.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// HandleHandshake verifies authentication with companion plugin and syncs settings.
func (h *HandshakeManager) HandleHandshake(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req HandshakeRequest
	secretHeader := r.Header.Get("X-JellyProxy-Secret")

	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
	}

	incomingSecret := req.Secret
	if incomingSecret == "" {
		incomingSecret = secretHeader
	}

	if h.cfg.SharedSecret != "" {
		a := sha256.Sum256([]byte(incomingSecret))
		b := sha256.Sum256([]byte(h.cfg.SharedSecret))
		if subtle.ConstantTimeCompare(a[:], b[:]) != 1 {
			log.Printf("[JellyProxy] Handshake refused: secret mismatch from %s", r.RemoteAddr)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":  "unauthorized",
				"message": "Shared secret does not match configured proxy secret",
			})
			return
		}
	}

	h.mu.Lock()
	h.paired = true
	h.lastSeen = time.Now().UTC()
	if req.ServerName != "" {
		h.serverName = req.ServerName
	}
	if req.ServerID != "" {
		h.serverID = req.ServerID
	}
	if req.JellyVersion != "" {
		h.jellyVersion = req.JellyVersion
	}
	abrMgr := h.abr
	storageMgr := h.storage
	h.mu.Unlock()

	if abrMgr != nil {
		if req.ABR != nil {
			abrMgr.UpdateConfig(*req.ABR)
		}
		if len(req.Encoders) > 0 {
			abrMgr.Capabilities().SetEncoders(req.Encoders)
		}
	}
	if storageMgr != nil && req.MaxStorageGB > 0 {
		if err := storageMgr.SetMaxStorageGB(req.MaxStorageGB); err == nil {
			h.cfg.MaxStorageGB = req.MaxStorageGB
		}
	}

	log.Printf("[JellyProxy] Handshake successful! Jellyfin '%s' (v%s, id: %s) paired at %s",
		req.ServerName, req.JellyVersion, req.ServerID, h.lastSeen.Format(time.RFC3339))

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":      "authenticated",
		"version":     "0.1.0",
		"message":     "JellyProxy successfully recognized and paired with Jellyfin companion plugin",
		"paired_at":   h.lastSeen,
		"upstream_ok": true,
	})
}
