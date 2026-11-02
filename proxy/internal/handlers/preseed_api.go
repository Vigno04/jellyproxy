package handlers

import (
 "encoding/json"
 "net/http"
 "github.com/Vigno04/jellyproxy/proxy/internal/auth"
 "github.com/Vigno04/jellyproxy/proxy/internal/config"
 "github.com/Vigno04/jellyproxy/proxy/internal/preseed"
)
type PreseedAPI struct { manager *preseed.PreseedManager; secret string }
func NewPreseedAPI(cfg *config.Config, m *preseed.PreseedManager) *PreseedAPI{return &PreseedAPI{manager:m,secret:cfg.SharedSecret}}
func (a *PreseedAPI) Start(w http.ResponseWriter,r *http.Request){if r.Method!=http.MethodPost{http.Error(w,"Method Not Allowed",405);return};var v struct{ID string `json:"id"`;ItemName string `json:"item_name"`;URLs []string `json:"urls"`;Headers http.Header `json:"headers"`};if json.NewDecoder(r.Body).Decode(&v)!=nil||len(v.URLs)==0{writeJSON(w,400,map[string]string{"error":"urls are required"});return};if err:=a.manager.StartWithOptions(v.ID,v.ItemName,v.URLs,v.Headers);err!=nil{writeJSON(w,409,map[string]string{"error":err.Error()});return};writeJSON(w,202,map[string]string{"status":"started","id":v.ID})}
func (a *PreseedAPI) Jobs(w http.ResponseWriter,r *http.Request){writeJSON(w,200,a.manager.Jobs())}
func (a *PreseedAPI) Cancel(w http.ResponseWriter,r *http.Request){if r.Method!=http.MethodPost{http.Error(w,"Method Not Allowed",405);return};var v struct{ID string `json:"id"`};if json.NewDecoder(r.Body).Decode(&v)!=nil||!a.manager.Cancel(v.ID){writeJSON(w,404,map[string]string{"error":"job not found"});return};writeJSON(w,200,map[string]string{"status":"cancelled"})}
func (a *PreseedAPI) Handler(fn http.HandlerFunc) http.HandlerFunc{return auth.RequireSecret(a.secret,fn)}
