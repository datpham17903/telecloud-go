package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"telecloud/config"
	"telecloud/database"

	"github.com/gin-gonic/gin"
)

func resetWarmState() {
	for _, m := range []*sync.Map{&warmedFiles, &warmFailures, &warmCurrentURL} {
		m.Range(func(k, _ any) bool { m.Delete(k); return true })
	}
}

// gatedWarm stubs warmFetch: every call records its URL and blocks until that
// URL's gate is closed, then reports size bytes.
type gatedWarm struct {
	mu    sync.Mutex
	urls  []string
	gates map[string]chan struct{}
	size  int64
}

func (g *gatedWarm) gate(url string) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.gates == nil {
		g.gates = map[string]chan struct{}{}
	}
	if g.gates[url] == nil {
		g.gates[url] = make(chan struct{})
	}
	return g.gates[url]
}

func (g *gatedWarm) fetch(_ context.Context, url string) (int64, error) {
	g.mu.Lock()
	g.urls = append(g.urls, url)
	g.mu.Unlock()
	<-g.gate(url)
	return g.size, nil
}

func (g *gatedWarm) called() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.urls...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestWarmOnShareCreation(t *testing.T) {
	r, cfg := newPrewarmRouter(t)
	resetWarmState()
	g := &gatedWarm{size: 60 << 20}
	warmFetch = g.fetch
	h := &Handler{cfg: cfg}
	r.POST("/files/:id/share", func(c *gin.Context) { c.Set("username", "u"); h.handleShareFile(c) })

	res, err := database.DB.Exec("INSERT INTO files (message_id, filename, path, size, mime_type, owner) VALUES (1, 'my clip #1.mp4', '/u', ?, 'video/mp4', 'u')", 60<<20)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()

	req := httptest.NewRequest(http.MethodPost, "/files/"+strconv.Itoa(int(id))+"/share", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req) // must return while the warm is still blocked
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var token string
	if err := database.RODB.Get(&token, "SELECT share_token FROM files WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	want := "https://" + prewarmHost + "/s/" + token + "/stream/my%20clip%20%231.mp4"
	waitFor(t, "warm start", func() bool { return len(g.called()) == 1 })
	if got := g.called()[0]; got != want {
		t.Errorf("warm URL = %q, want %q", got, want)
	}
	close(g.gate(want))
	waitFor(t, "warm done", func() bool { _, ok := warmedFiles.Load(int(id)); return ok })
}

func TestWarmShareSkipped(t *testing.T) {
	_, cfg := newPrewarmRouter(t)
	resetWarmState()
	var calls int32
	warmFetch = func(context.Context, string) (int64, error) { atomic.AddInt32(&calls, 1); return 0, nil }
	mime := func(s string) *string { return &s }
	pw := "hash"
	base := database.File{ID: 7, Filename: "a.mp4", Size: 10 << 20, MimeType: mime("video/mp4")}
	tests := []struct {
		name string
		mod  func(f *database.File, c *config.Config)
	}{
		{"password", func(f *database.File, _ *config.Config) { f.SharePassword = &pw }},
		{"folder", func(f *database.File, _ *config.Config) { f.IsFolder = true }},
		{"non-video", func(f *database.File, _ *config.Config) { f.Filename = "a.zip"; f.MimeType = mime("application/zip") }},
		{"too large", func(f *database.File, _ *config.Config) { f.Size = discordCFMaxCacheBytes + 1 }},
		{"empty base", func(_ *database.File, c *config.Config) { c.DiscordCFBaseURL = "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := *cfg
			f := base
			tc.mod(&f, &c)
			(&Handler{cfg: &c}).warmShareInBackground(f, "tok")
			time.Sleep(50 * time.Millisecond)
			if n := atomic.LoadInt32(&calls); n != 0 {
				t.Errorf("warm calls = %d, want 0", n)
			}
			if _, busy := warmFlights.Load(7); busy {
				t.Error("flight started")
			}
		})
	}
}

func TestReshareClearsWarmedRecord(t *testing.T) {
	r, cfg := newPrewarmRouter(t)
	resetWarmState()
	g := &gatedWarm{size: 60 << 20}
	warmFetch = g.fetch
	discordWarmWait = 50 * time.Millisecond
	h := &Handler{cfg: cfg}
	mime := "video/mp4"
	item := database.File{ID: 1, Filename: "clip.mp4", Size: 60 << 20, MimeType: &mime}

	warmedFiles.Store(1, time.Now()) // from a previous share link
	h.warmShareInBackground(item, "vid")
	if _, ok := warmedFiles.Load(1); ok {
		t.Fatal("warmed record survived re-share")
	}
	body := prewarmGet(r, "vid", true)
	if strings.Contains(body, "og:video") {
		t.Errorf("expected card while the new URL is warming, got %s", body)
	}
	close(g.gate(prewarmVideoURL))
	waitFor(t, "warm done", func() bool { _, ok := warmedFiles.Load(1); return ok })
	if body := prewarmGet(r, "vid", true); !strings.Contains(body, "og:video") {
		t.Errorf("expected og:video once warmed, got %s", body)
	}
}

func TestStaleWarmDoesNotMarkNewShare(t *testing.T) {
	_, cfg := newPrewarmRouter(t)
	resetWarmState()
	g := &gatedWarm{size: 60 << 20}
	warmFetch = g.fetch
	h := &Handler{cfg: cfg}
	mime := "video/mp4"
	item := database.File{ID: 1, Filename: "clip.mp4", Size: 60 << 20, MimeType: &mime}

	oldURL := discordMediaURL(cfg.DiscordCFBaseURL, "old", "clip.mp4")
	newURL := discordMediaURL(cfg.DiscordCFBaseURL, "new", "clip.mp4")
	h.warmShareInBackground(item, "old")
	waitFor(t, "old warm start", func() bool { return len(g.called()) == 1 })
	h.warmShareInBackground(item, "new") // re-share while the old warm is running
	waitFor(t, "new warm start", func() bool { return len(g.called()) == 2 })

	close(g.gate(oldURL))
	time.Sleep(100 * time.Millisecond)
	if _, ok := warmedFiles.Load(1); ok {
		t.Fatal("stale warm marked the file as warmed")
	}
	close(g.gate(newURL))
	waitFor(t, "new warm done", func() bool { _, ok := warmedFiles.Load(1); return ok })
}
