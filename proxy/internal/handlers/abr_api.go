package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/Vigno04/jellyproxy/proxy/internal/abr"
)

// ABRAPI exposes the bandwidth shaper to the companion plugin.
type ABRAPI struct{ Manager *abr.BandwidthManager }

// Config handles GET (read) and POST (update) of the shaper configuration.
func (a *ABRAPI) Config(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, a.Manager.Config())
	case http.MethodPost:
		var c abr.Config
		if json.NewDecoder(r.Body).Decode(&c) != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		a.Manager.UpdateConfig(c)
		writeJSON(w, http.StatusOK, a.Manager.Config())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// Status returns the live pipe table.
func (a *ABRAPI) Status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.Manager.GetStatus())
}

// Capabilities lets the plugin report which video codecs the home server can encode
// (POST {"encoders":["h264","hevc"]}), enabling codec convergence (spec case 7).
func (a *ABRAPI) Capabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var v struct {
			Encoders []string `json:"encoders"`
		}
		if json.NewDecoder(r.Body).Decode(&v) != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		a.Manager.Capabilities().SetEncoders(v.Encoders)
	} else if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"encoders": a.Manager.Capabilities().Encoders()})
}
