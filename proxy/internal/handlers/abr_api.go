package handlers

import (
 "encoding/json"
 "net/http"
 "github.com/Vigno04/jellyproxy/proxy/internal/abr"
)
type ABRAPI struct { Manager *abr.BandwidthManager }
func (a *ABRAPI) Config(w http.ResponseWriter,r *http.Request){ if r.Method==http.MethodGet {json.NewEncoder(w).Encode(a.Manager.Config());return}; if r.Method!=http.MethodPost {http.Error(w,"method not allowed",405);return}; var c abr.Config; if json.NewDecoder(r.Body).Decode(&c)!=nil {http.Error(w,"invalid json",400);return};a.Manager.UpdateConfig(c);json.NewEncoder(w).Encode(c) }
func (a *ABRAPI) Status(w http.ResponseWriter,r *http.Request){json.NewEncoder(w).Encode(a.Manager.GetStatus())}
