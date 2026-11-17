package api

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"telecloud/config"
	"telecloud/database"

	"github.com/gin-gonic/gin"
)

const (
	prewarmDiscordUA = "Mozilla/5.0 (compatible; Discordbot/2.0; +https://discordapp.com)"
	prewarmHost      = "telecloud-cf.example.com"
	prewarmVideoURL  = "https://" + prewarmHost + "/s/vid/stream/clip.mp4"
)

func newPrewarmRouter(t *testing.T) (*gin.Engine, *config.Config) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dbPath := filepath.Join(t.TempDir(), "prewarm.db")
	t.Cleanup(func() {
		database.RWDB.Close()
		database.RODB.Close()
	})
	if err := database.InitDB("sqlite", dbPath, ""); err != nil {
		t.Fatal(err)
	}

	rows := []struct {
		token string
		size  int64
		pw    *string
	}{
		{"vid", 60 << 20, nil},
		{"huge", 600 << 20, nil},
		{"pw", 10 << 20, func() *string { s := "x"; return &s }()},
	}
	for _, row := range rows {
		_, err := database.DB.Exec("INSERT INTO files (message_id, filename, path, size, mime_type, share_token, thumb_path, share_password) VALUES (1, 'clip.mp4', ?, ?, 'video/mp4', ?, ?, ?)",
			"/"+row.token, row.size, row.token, *embedTestThumb(t, 320, 180), row.pw)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.DB.Exec("INSERT INTO share_sessions (token, share_token, expires_at) VALUES ('sess', 'pw', ?)", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	tmpl := template.Must(template.New("share.html").Parse("SHARE_PAGE"))
	r := gin.New()
	r.SetHTMLTemplate(tmpl)
	cfg := &config.Config{DiscordEmbedMaxVideoMB: 1024, DiscordCFBaseURL: "https://" + prewarmHost}
	h := &Handler{cfg: cfg}
	r.GET("/s/:token", h.handleGetSharedFile)
	r.GET("/s/:token/stream/:filename", h.handleStreamSharedFile)

	prevWarm, prevWait, prevServe := warmFetch, discordWarmWait, serveTelegramFile
	t.Cleanup(func() { warmFetch, discordWarmWait, serveTelegramFile = prevWarm, prevWait, prevServe })
	warmedFiles.Range(func(k, _ any) bool { warmedFiles.Delete(k); return true })
	warmFailures.Range(func(k, _ any) bool { warmFailures.Delete(k); return true })
	return r, cfg
}

func prewarmGet(r *gin.Engine, token string, cfRay bool) string {
	host := "example.com"
	if cfRay {
		host = prewarmHost
	}
	return prewarmGetHost(r, token, cfRay, host)
}

func prewarmGetHost(r *gin.Engine, token string, cfRay bool, host string) string {
	req := httptest.NewRequest(http.MethodGet, "/s/"+token, nil)
	req.Header.Set("User-Agent", prewarmDiscordUA)
	req.Host = host
	req.Header.Set("X-Forwarded-Proto", "https")
	if cfRay {
		req.Header.Set("Cf-Ray", "abc123-SIN")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Body.String()
}

func TestStreamSharedFileCacheControl(t *testing.T) {
	r, _ := newPrewarmRouter(t)
	serveTelegramFile = func(_ *http.Request, w http.ResponseWriter, _ database.File, _ *config.Config) error {
		w.WriteHeader(http.StatusOK)
		return nil
	}

	req := httptest.NewRequest(http.MethodGet, "/s/vid/stream/clip.mp4", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if got := w.Header().Get("Cache-Control"); got != "public, max-age=86400" {
		t.Errorf("no-password Cache-Control = %q", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/s/pw/stream/clip.mp4", nil)
	req.AddCookie(&http.Cookie{Name: "share_auth_pw", Value: "sess"})
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("password stream status = %d", w.Code)
	}
	if got := w.Header().Get("Cache-Control"); strings.Contains(got, "public") {
		t.Errorf("password share must not be public, got %q", got)
	}
}

func TestDiscordPrewarm(t *testing.T) {
	r, cfg := newPrewarmRouter(t)
	var calls int32
	setWarm := func(delay time.Duration, wait time.Duration) {
		atomic.StoreInt32(&calls, 0)
		warmedFiles.Range(func(k, _ any) bool { warmedFiles.Delete(k); return true })
		warmFailures.Range(func(k, _ any) bool { warmFailures.Delete(k); return true })
		discordWarmWait = wait
		warmFetch = func(_ context.Context, url string) (int64, error) {
			atomic.AddInt32(&calls, 1)
			time.Sleep(delay)
			return 60 << 20, nil
		}
	}
	waitIdle := func() {
		for i := 0; i < 300; i++ {
			if _, busy := warmFlights.Load(1); !busy {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("warm did not finish")
	}

	t.Run("fast warm serves video", func(t *testing.T) {
		setWarm(0, 2*time.Second)
		body := prewarmGet(r, "vid", true)
		if !strings.Contains(body, `og:video" content="`+prewarmVideoURL+`"`) {
			t.Errorf("expected og:video, got %s", body)
		}
		// Second request is served from the warmed record without refetching.
		prewarmGet(r, "vid", true)
		if n := atomic.LoadInt32(&calls); n != 1 {
			t.Errorf("warm calls = %d, want 1", n)
		}
	})

	t.Run("slow warm serves card then video", func(t *testing.T) {
		setWarm(300*time.Millisecond, 50*time.Millisecond)
		body := prewarmGet(r, "vid", true)
		if strings.Contains(body, "og:video") {
			t.Errorf("unexpected og:video: %s", body)
		}
		for _, want := range []string{`og:image" content="https://telecloud-cf.example.com/s/vid/thumb"`, "summary_large_image", `og:image:width" content="1280"`} {
			if !strings.Contains(body, want) {
				t.Errorf("card missing %q\n%s", want, body)
			}
		}
		waitIdle()
		discordWarmWait = 2 * time.Second
		if body := prewarmGet(r, "vid", true); !strings.Contains(body, "og:video") {
			t.Errorf("expected og:video after warm, got %s", body)
		}
	})

	t.Run("too large for cloudflare is served without warming", func(t *testing.T) {
		setWarm(0, 2*time.Second)
		body := prewarmGet(r, "huge", true)
		if !strings.Contains(body, "og:video") {
			t.Errorf("expected og:video, got %s", body)
		}
		if n := atomic.LoadInt32(&calls); n != 0 {
			t.Errorf("warm calls = %d, want 0", n)
		}
	})

	t.Run("no Cf-Ray keeps old behavior", func(t *testing.T) {
		setWarm(0, 2*time.Second)
		if body := prewarmGet(r, "vid", false); !strings.Contains(body, "og:video") {
			t.Errorf("expected og:video, got %s", body)
		}
		if n := atomic.LoadInt32(&calls); n != 0 {
			t.Errorf("warm calls = %d, want 0", n)
		}
	})

	t.Run("concurrent requests share one warm", func(t *testing.T) {
		setWarm(200*time.Millisecond, 2*time.Second)
		var wg sync.WaitGroup
		bodies := make([]string, 2)
		for i := range bodies {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				bodies[i] = prewarmGet(r, "vid", true)
			}(i)
		}
		wg.Wait()
		for _, b := range bodies {
			if !strings.Contains(b, "og:video") {
				t.Errorf("expected og:video, got %s", b)
			}
		}
		if n := atomic.LoadInt32(&calls); n != 1 {
			t.Errorf("warm calls = %d, want 1", n)
		}
	})

	t.Run("warm URL is built from config", func(t *testing.T) {
		setWarm(0, 2*time.Second)
		var got string
		warmFetch = func(_ context.Context, url string) (int64, error) {
			got = url
			return 60 << 20, nil
		}
		prewarmGet(r, "vid", true)
		if got != prewarmVideoURL {
			t.Errorf("warm URL = %q, want %q", got, prewarmVideoURL)
		}
	})

	t.Run("host mismatch does not warm", func(t *testing.T) {
		setWarm(0, 2*time.Second)
		body := prewarmGetHost(r, "vid", true, "evil.example")
		if n := atomic.LoadInt32(&calls); n != 0 {
			t.Errorf("warm calls = %d, want 0", n)
		}
		if !strings.Contains(body, `og:video" content="https://evil.example/s/vid/stream/clip.mp4"`) {
			t.Errorf("expected unchanged embed, got %s", body)
		}
	})

	t.Run("empty base URL disables warm", func(t *testing.T) {
		setWarm(0, 2*time.Second)
		cfg.DiscordCFBaseURL = ""
		defer func() { cfg.DiscordCFBaseURL = "https://" + prewarmHost }()
		body := prewarmGet(r, "vid", true)
		if n := atomic.LoadInt32(&calls); n != 0 {
			t.Errorf("warm calls = %d, want 0", n)
		}
		if !strings.Contains(body, "og:video") {
			t.Errorf("expected og:video, got %s", body)
		}
	})

	t.Run("failed warm is cached", func(t *testing.T) {
		setWarm(0, 2*time.Second)
		warmFetch = func(_ context.Context, _ string) (int64, error) {
			atomic.AddInt32(&calls, 1)
			return 1, nil // wrong size
		}
		for i := 0; i < 2; i++ {
			body := prewarmGet(r, "vid", true)
			if strings.Contains(body, "og:video") || !strings.Contains(body, "summary_large_image") {
				t.Errorf("expected card, got %s", body)
			}
			waitIdle()
		}
		if n := atomic.LoadInt32(&calls); n != 1 {
			t.Errorf("warm calls = %d, want 1", n)
		}
	})
}

func TestStreamSharedFileErrorNotCacheable(t *testing.T) {
	r, _ := newPrewarmRouter(t)
	serveTelegramFile = func(*http.Request, http.ResponseWriter, database.File, *config.Config) error {
		return errors.New("telegram down")
	}
	req := httptest.NewRequest(http.MethodGet, "/s/vid/stream/clip.mp4", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}
