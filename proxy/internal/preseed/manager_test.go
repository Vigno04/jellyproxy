package preseed

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Vigno04/jellyproxy/proxy/internal/cache"
)

func TestManagerStart(t *testing.T) {
	s, err := cache.NewStorageManager(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(s, "http://127.0.0.1:1", "")
	if err := m.Start("j", []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if len(m.Jobs()) != 1 {
		t.Fatal("expected one job")
	}
	if err := m.Start("j", []string{"x"}); err == nil {
		t.Fatal("duplicate job id must be rejected")
	}
}

func TestPlaylistPreseedDownloadsSegments(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/main.m3u8", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-MAP:URI=init.mp4\n#EXTINF:1,\nchunk-1.m4s\n"))
	})
	mux.HandleFunc("/init.mp4", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("init")) })
	mux.HandleFunc("/chunk-1.m4s", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("segment")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s, err := cache.NewStorageManager(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(s, srv.URL, "")
	if err := m.Start("playlist", []string{"/main.m3u8"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		j := m.Jobs()[0]
		if j.Status != "running" {
			if j.Status != "completed" || j.TotalSegments != 2 || j.Completed != 2 {
				t.Fatalf("unexpected job: %+v", j)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job did not finish")
}
