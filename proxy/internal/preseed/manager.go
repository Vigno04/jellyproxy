package preseed

import (
    "bufio"
    "context"
    "fmt"
    "io"
    "net/http"
    "net/url"
    "path/filepath"
    "strings"
    "sync"
    "sync/atomic"
    "time"

    "github.com/Vigno04/jellyproxy/proxy/internal/cache"
)

type Job struct {
    ID string `json:"id"`
    ItemName string `json:"item_name,omitempty"`
    URLs []string `json:"urls,omitempty"`
    TotalSegments int `json:"total_segments"`
    Completed int `json:"completed"`
    Status string `json:"status"`
    Error string `json:"error,omitempty"`
}

type PreseedManager struct { storage *cache.StorageManager; upstream, host string; active *int32; client *http.Client; mu sync.Mutex; jobs map[string]*Job; cancels map[string]context.CancelFunc }
func NewManager(storage *cache.StorageManager, upstream, host string, active *int32) *PreseedManager { return &PreseedManager{storage:storage, upstream:strings.TrimRight(upstream,"/"),host:host,active:active,client:&http.Client{Timeout:2*time.Minute},jobs:map[string]*Job{},cancels:map[string]context.CancelFunc{}} }

func (m *PreseedManager) Start(id string, urls []string) error { return m.StartWithOptions(id, "", urls, nil) }
func (m *PreseedManager) StartWithOptions(id, itemName string, urls []string, headers http.Header) error {
    m.mu.Lock(); defer m.mu.Unlock()
    if id=="" { id=time.Now().UTC().Format("20060102T150405.000000000") }
    if _,ok:=m.jobs[id];ok { return fmt.Errorf("job already exists") }
    j:=&Job{ID:id,ItemName:itemName,URLs:append([]string(nil),urls...),Status:"running"}
    ctx,cancel:=context.WithCancel(context.Background()); m.jobs[id]=j; m.cancels[id]=cancel
    go m.run(ctx,j,headers); return nil
}
func (m *PreseedManager) run(ctx context.Context,j *Job, headers http.Header) {
    defer func(){ m.mu.Lock(); if j.Status=="running" { j.Status="completed" }; delete(m.cancels,j.ID); m.mu.Unlock() }()
    for _, raw := range j.URLs {
        if ctx.Err()!=nil { return }
        if m.active!=nil { for atomic.LoadInt32(m.active)>0 { select { case <-ctx.Done(): return; case <-time.After(250*time.Millisecond): } } }
        if strings.HasSuffix(strings.ToLower(strings.Split(raw,"?")[0]), ".m3u8") {
            if err:=m.processPlaylist(ctx,j,raw,headers); err!=nil { m.fail(j,err); return }
        } else if err:=m.download(ctx, j, raw, headers); err!=nil { m.fail(j,err); return }
    }
}
func (m *PreseedManager) processPlaylist(ctx context.Context,j *Job, raw string, headers http.Header) error {
    body, err:=m.fetch(ctx,raw,headers); if err!=nil{return err}
    base,err:=url.Parse(m.upstreamURL(raw)); if err!=nil{return err}
    seen:=map[string]bool{}
    scanner:=bufio.NewScanner(strings.NewReader(string(body)))
    for scanner.Scan() { line:=strings.TrimSpace(scanner.Text()); var ref string
        if strings.HasPrefix(line,"#EXT-X-MAP:") { for _, part:=range strings.Split(strings.TrimPrefix(line,"#EXT-X-MAP:"),",") { if strings.HasPrefix(part,"URI=") { ref=strings.Trim(strings.TrimPrefix(part,"URI="),"\"") } } } else if line!=""&&!strings.HasPrefix(line,"#") { ref=line }
        if ref=="" {continue}; u,err:=base.Parse(ref); if err!=nil{continue}; absolute:=u.String(); if seen[absolute]{continue}; seen[absolute]=true
        if !isSegment(absolute){continue}; m.mu.Lock(); j.TotalSegments++; m.mu.Unlock(); if err:=m.download(ctx,j,absolute,headers);err!=nil{return err}
    }
    return scanner.Err()
}
func isSegment(raw string) bool { ext:=strings.ToLower(filepath.Ext(strings.Split(raw,"?")[0])); return ext==".m4s"||ext==".ts"||ext==".mp4"||ext==".m4a"||ext==".aac" }
func (m *PreseedManager) upstreamURL(raw string) string { if strings.HasPrefix(raw,"http://")||strings.HasPrefix(raw,"https://"){return raw}; return m.upstream+"/"+strings.TrimLeft(raw,"/") }
func (m *PreseedManager) fetch(ctx context.Context,raw string,h http.Header)([]byte,error){ req,e:=http.NewRequestWithContext(ctx,http.MethodGet,m.upstreamURL(raw),nil);if e!=nil{return nil,e};m.apply(req,h);resp,e:=m.client.Do(req);if e!=nil{return nil,e};defer resp.Body.Close();if resp.StatusCode/100!=2{return nil,fmt.Errorf("upstream returned %s",resp.Status)};return io.ReadAll(resp.Body) }
func (m *PreseedManager) download(ctx context.Context,j *Job,raw string,h http.Header) error { key:=cache.HashKey(cache.BuildKey(cache.CategoryMedia,raw));if m.storage.Has(key){m.increment(j);return nil};req,e:=http.NewRequestWithContext(ctx,http.MethodGet,m.upstreamURL(raw),nil);if e!=nil{return e};m.apply(req,h);resp,e:=m.client.Do(req);if e!=nil{return e};defer resp.Body.Close();if resp.StatusCode/100!=2{return fmt.Errorf("upstream returned %s",resp.Status)};tmp,e:=m.storage.CreateTempFile();if e!=nil{return e};p:=tmp.Name();_,e=io.Copy(tmp,resp.Body);tmp.Close();if e==nil{e=m.storage.CommitTempFile(p,cache.CacheMetadata{Key:key,URL:raw,Category:cache.CategoryMedia,ContentType:resp.Header.Get("Content-Type"),Headers:resp.Header,CreatedAt:time.Now().UTC()})};if e!=nil{_ = m.storage.Delete(key);return e};m.increment(j);return nil }
func (m *PreseedManager) apply(req *http.Request,h http.Header){for k,v:=range h{for _,x:=range v{req.Header.Add(k,x)}};if m.host!=""{req.Host=m.host}}
func (m *PreseedManager) increment(j *Job){m.mu.Lock();j.Completed++;m.mu.Unlock()}
func (m *PreseedManager) fail(j *Job,e error){m.mu.Lock();j.Status="failed";j.Error=e.Error();m.mu.Unlock()}
func (m *PreseedManager) Jobs() []*Job {m.mu.Lock();defer m.mu.Unlock();out:=make([]*Job,0,len(m.jobs));for _,j:=range m.jobs{c:=*j;c.URLs=append([]string(nil),j.URLs...);out=append(out,&c)};return out}
func (m *PreseedManager) Cancel(id string) bool {m.mu.Lock();defer m.mu.Unlock();if c:=m.cancels[id];c!=nil{c();if j:=m.jobs[id];j!=nil{j.Status="cancelled"};return true};return false}
