package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/jellyproxy-project/jellyproxy/proxy/internal/config"
)

// HandshakeManager tracks paired Jellyfin server state
type HandshakeManager struct {
	cfg          *config.Config
	mu           sync.RWMutex
	paired       bool
	lastSeen     time.Time
	serverName   string
	serverID     string
	jellyVersion string
}

// HandshakeRequest payload from Jellyfin companion plugin
type HandshakeRequest struct {
	Secret        string `json:"secret"`
	ServerName    string `json:"server_name"`
	ServerID      string `json:"server_id"`
	PluginVersion string `json:"plugin_version"`
	JellyVersion  string `json:"jellyfin_version"`
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

// NewHandshakeManager initializes handshake tracker
func NewHandshakeManager(cfg *config.Config) *HandshakeManager {
	return &HandshakeManager{
		cfg: cfg,
	}
}

// HandleStatus returns JSON status of the proxy
func (h *HandshakeManager) HandleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	h.mu.RLock()
	resp := StatusResponse{
		Status:        "ok",
		Version:       "0.1.0",
		UpstreamURL:   h.cfg.UpstreamURL,
		Paired:        h.paired,
		LastSeen:      h.lastSeen,
		ServerName:    h.serverName,
		ServerID:      h.serverID,
		SecretSecured: h.cfg.SharedSecret != "",
	}
	h.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// HandleHandshake verifies authentication with companion plugin
func (h *HandshakeManager) HandleHandshake(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req HandshakeRequest
	// Support secret via Header or JSON Body
	secretHeader := r.Header.Get("X-JellyProxy-Secret")

	if r.Header.Get("Content-Type") == "application/json" {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
	}

	incomingSecret := req.Secret
	if incomingSecret == "" {
		incomingSecret = secretHeader
	}

	// Secret validation
	if h.cfg.SharedSecret != "" && incomingSecret != h.cfg.SharedSecret {
		log.Printf("[JellyProxy] Handshake refused: secret mismatch from %s", r.RemoteAddr)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "unauthorized",
			"message": "Shared secret does not match configured proxy secret",
		})
		return
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
	h.mu.Unlock()

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
