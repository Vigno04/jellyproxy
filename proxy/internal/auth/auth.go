package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

func ExtractSecret(r *http.Request) string {
	if v := r.Header.Get("X-JellyProxy-Secret"); v != "" { return v }
	if v := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(v), "bearer ") { if s := strings.TrimSpace(v[7:]); s != "" { return s } }
	return r.URL.Query().Get("secret")
}

func IsAuthorized(r *http.Request, expectedSecret string) bool {
	if expectedSecret == "" { return true }
	a, b := sha256.Sum256([]byte(ExtractSecret(r))), sha256.Sum256([]byte(expectedSecret))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func RequireSecret(expectedSecret string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !IsAuthorized(r, expectedSecret) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"status":"unauthorized", "message":"Invalid or missing shared secret"})
			return
		}
		next(w, r)
	}
}
