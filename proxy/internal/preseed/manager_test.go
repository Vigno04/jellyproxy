package preseed

import (
    "net/http"
    "net/http/httptest"
    "sync/atomic"
    "testing"
    "time"
    "github.com/jellyproxy-project/jellyproxy/proxy/internal/cache"
    "os"
)
func TestManagerStart(t *testing.T){d:=t.TempDir();s,e:=cache.NewStorageManager(d,1);if e!=nil{t.Fatal(e)};var n int32;m:=NewManager(s,"http://127.0.0.1:1","",&n);if e=m.Start("j",[]string{"x"});e!=nil{t.Fatal(e)};if len(m.Jobs())!=1{t.Fatal()}; _ = os.RemoveAll(d);atomic.StoreInt32(&n,1)}
func TestPlaylistPreseedDownloadsSegments(t *testing.T) {
 mux:=http.NewServeMux(); mux.HandleFunc("/main.m3u8",func(w http.ResponseWriter,r *http.Request){_,_=w.Write([]byte("#EXTM3U\n#EXT-X-MAP:URI=init.mp4\n#EXTINF:1,\nchunk-1.m4s\n"))}); mux.HandleFunc("/init.mp4",func(w http.ResponseWriter,r *http.Request){_,_=w.Write([]byte("init"))}); mux.HandleFunc("/chunk-1.m4s",func(w http.ResponseWriter,r *http.Request){_,_=w.Write([]byte("segment"))})
 srv:=httptest.NewServer(mux); defer srv.Close(); s,e:=cache.NewStorageManager(t.TempDir(),1);if e!=nil{t.Fatal(e)};m:=NewManager(s,srv.URL,"",nil);if e=m.Start("playlist",[]string{"/main.m3u8"});e!=nil{t.Fatal(e)}
 for i:=0;i<100;i++ {j:=m.Jobs()[0];if j.Status!="running" {if j.Status!="completed"||j.TotalSegments!=2||j.Completed!=2{t.Fatalf("unexpected job: %+v",j)};return};time.Sleep(10*time.Millisecond)};t.Fatal("job did not finish")
}
