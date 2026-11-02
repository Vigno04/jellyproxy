// Package abr implements adaptive upload bandwidth allocation for JellyProxy.
package abr

import (
	"strings"
	"sync"
	"time"
)

const idleTimeout = time.Minute

type Config struct {
	Enabled bool `json:"enabled"`
	UploadSpeedBps int64 `json:"uploadSpeedBps"`
	HeadroomPercent int `json:"headroomPercent"`
	SingleClientCeilingBps int64 `json:"singleClientCeilingBps"`
	MinBitrateBps int64 `json:"minBitrateBps"`
	Policy string `json:"policy"`
	AllowDegradingExistingStreams bool `json:"allowDegradingExistingStreams"`
}

type PipeStatus struct { ItemID string `json:"itemId"`; Clients int `json:"clients"`; RequestedBps int64 `json:"requestedBps"`; AllocatedBps int64 `json:"allocatedBps"`; LastSeen time.Time `json:"lastSeen"` }
type Status struct { Config Config `json:"config"`; TotalBps int64 `json:"totalBps"`; UsedBps int64 `json:"usedBps"`; ActivePipes int `json:"activePipes"`; Pipes []PipeStatus `json:"pipes"` }

type pipe struct { item string; clients map[string]bool; requested, allocated int64; lastSeen time.Time }
type BandwidthManager struct { mu sync.Mutex; cfg Config; pipes map[string]*pipe; clientItem map[string]string }

func normalize(c Config) Config { if c.HeadroomPercent < 0 { c.HeadroomPercent=0 }; if c.HeadroomPercent > 99 { c.HeadroomPercent=99 }; if c.MinBitrateBps <= 0 { c.MinBitrateBps=1500000 }; if c.Policy != "FirstComeFirstServed" { c.Policy="FairShare" }; return c }
func NewBandwidthManager(c Config) *BandwidthManager { return &BandwidthManager{cfg:normalize(c), pipes:make(map[string]*pipe), clientItem:make(map[string]string)} }
func (m *BandwidthManager) cleanup(now time.Time) { for id,p := range m.pipes { if now.Sub(p.lastSeen)>idleTimeout { for c:=range p.clients { delete(m.clientItem,c) }; delete(m.pipes,id) } } }
func (m *BandwidthManager) capacity() int64 { if m.cfg.UploadSpeedBps<=0{return 0}; return m.cfg.UploadSpeedBps*int64(100-m.cfg.HeadroomPercent)/100 }
func (m *BandwidthManager) Allocate(clientID,itemID string, requested int64) int64 { m.mu.Lock(); defer m.mu.Unlock(); now:=time.Now(); m.cleanup(now); if clientID=="" {clientID="anonymous"}; if old:=m.clientItem[clientID]; old!="" && old!=itemID { if p:=m.pipes[old]; p!=nil {delete(p.clients,clientID); if len(p.clients)==0 {delete(m.pipes,old)}} }; p:=m.pipes[itemID]; if p==nil {p=&pipe{item:itemID,clients:make(map[string]bool)}; m.pipes[itemID]=p}; p.clients[clientID]=true; if requested>p.requested {p.requested=requested}; p.lastSeen=now; m.clientItem[clientID]=itemID; m.reallocate(); return p.allocated }
func (m *BandwidthManager) reallocate() { cap:=m.capacity(); if !m.cfg.Enabled { for _,p:=range m.pipes {p.allocated=p.requested}; return }; if m.cfg.SingleClientCeilingBps>0 && len(m.pipes)==1 && cap>m.cfg.SingleClientCeilingBps {cap=m.cfg.SingleClientCeilingBps}; if m.cfg.Policy=="FirstComeFirstServed" { for _,p:=range m.pipes {p.allocated=0; if cap<=0 {continue}; p.allocated=min64(p.requested,cap); cap-=p.allocated } ; return }; for _,p:=range m.pipes {p.allocated=0}; remaining:=cap; active:=make(map[*pipe]bool); for _,p:=range m.pipes {active[p]=true}; for len(active)>0 && remaining>0 { share:=remaining/int64(len(active)); if share<1 {share=1}; progress:=false; for p:=range active { want:=p.requested-p.allocated; if want<=share {p.allocated+=want; remaining-=want; delete(active,p); progress=true} }; if !progress {for p:=range active {p.allocated+=min64(p.requested-p.allocated,share)}; break} }; for _,p:=range m.pipes {if p.allocated>0 && p.allocated<m.cfg.MinBitrateBps && p.requested>=m.cfg.MinBitrateBps && m.cfg.AllowDegradingExistingStreams {p.allocated=m.cfg.MinBitrateBps} } }
func min64(a,b int64)int64{if a<b{return a};return b}
func (m *BandwidthManager) Release(clientID string) { m.mu.Lock(); defer m.mu.Unlock(); if id:=m.clientItem[clientID]; id!="" {if p:=m.pipes[id];p!=nil {delete(p.clients,clientID);p.lastSeen=time.Now();if len(p.clients)==0 {delete(m.pipes,id)}};delete(m.clientItem,clientID);m.reallocate()} }
func (m *BandwidthManager) GetStatus() Status { m.mu.Lock(); defer m.mu.Unlock(); m.cleanup(time.Now()); s:=Status{Config:m.cfg,TotalBps:m.capacity()}; for _,p:=range m.pipes {s.ActivePipes++;s.UsedBps+=p.allocated;s.Pipes=append(s.Pipes,PipeStatus{ItemID:p.item,Clients:len(p.clients),RequestedBps:p.requested,AllocatedBps:p.allocated,LastSeen:p.lastSeen})}; return s }
func (m *BandwidthManager) UpdateConfig(c Config) {m.mu.Lock();defer m.mu.Unlock();m.cfg=normalize(c);m.reallocate()}
func (m *BandwidthManager) Config() Config {m.mu.Lock();defer m.mu.Unlock();return m.cfg}
func SamePolicy(s string) bool{return strings.EqualFold(s,"FairShare")||strings.EqualFold(s,"FirstComeFirstServed")}
