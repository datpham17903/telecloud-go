package api

import (
	"html/template"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"telecloud/config"
	"telecloud/database"

	"github.com/gin-gonic/gin"
)

func embedTestThumb(t *testing.T, w, h int) *string {
	p := filepath.Join(t.TempDir(), "thumb.jpg")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return &p
}

func TestIsDiscordBot(t *testing.T) {
	if !isDiscordBot("Mozilla/5.0 (compatible; Discordbot/2.0; +https://discordapp.com)") {
		t.Error("Discord UA not detected")
	}
	if isDiscordBot("Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/130.0") {
		t.Error("browser UA detected as Discord")
	}
}

func TestBuildDiscordEmbed(t *testing.T) {
	const origin = "https://example.com"
	const token = "tok"
	const maxVideo = int64(80 << 20)
	created := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	missing := filepath.Join(t.TempDir(), "nope.jpg")
	mp4 := discordEmbed{
		ImageURL: origin + "/s/tok/thumb", VideoURL: origin + "/s/tok/stream/clip.mp4",
		VideoType: "video/mp4", VideoWidth: 1280, VideoHeight: 720,
	}
	esc := mp4
	esc.VideoURL = origin + "/s/tok/stream/phim%20hay%20%231%3F.mp4"

	tests := []struct {
		name, filename, mime string
		size                 int64
		thumb                *string
		ok                   bool
		want                 discordEmbed
	}{
		{"mp4 under limit", "clip.mp4", "video/mp4", 40 << 20, embedTestThumb(t, 320, 180), true, mp4},
		{"mp4 at limit", "clip.mp4", "video/mp4", 80 << 20, embedTestThumb(t, 320, 180), true, mp4},
		{"mp4 over limit", "clip.mp4", "video/mp4", 80<<20 + 1, embedTestThumb(t, 320, 180), false, discordEmbed{}},
		{"mov portrait", "a.mov", "video/quicktime", 10 << 20, embedTestThumb(t, 320, 569), true, discordEmbed{
			ImageURL: origin + "/s/tok/thumb", VideoURL: origin + "/s/tok/stream/a.mov",
			VideoType: "video/quicktime", VideoWidth: 1280, VideoHeight: 2276}},
		{"webm", "a.webm", "video/webm", 10 << 20, embedTestThumb(t, 320, 180), true, discordEmbed{
			ImageURL: origin + "/s/tok/thumb", VideoURL: origin + "/s/tok/stream/a.webm",
			VideoType: "video/webm", VideoWidth: 1280, VideoHeight: 720}},
		{"odd height", "a.mp4", "video/mp4", 10 << 20, embedTestThumb(t, 320, 181), true, discordEmbed{
			ImageURL: origin + "/s/tok/thumb", VideoURL: origin + "/s/tok/stream/a.mp4",
			VideoType: "video/mp4", VideoWidth: 1280, VideoHeight: 724}},
		{"svg", "a.svg", "image/svg+xml", 1 << 20, embedTestThumb(t, 320, 240), false, discordEmbed{}},
		{"gif", "a.gif", "image/gif", 1 << 20, embedTestThumb(t, 320, 240), true, discordEmbed{
			ImageURL: origin + "/s/tok/stream/a.gif", ImageWidth: 1280, ImageHeight: 960}},
		{"mkv", "a.mkv", "video/x-matroska", 10 << 20, embedTestThumb(t, 320, 180), false, discordEmbed{}},
		{"video without thumb", "clip.mp4", "video/mp4", 10 << 20, nil, false, discordEmbed{}},
		{"video thumb missing on disk", "clip.mp4", "video/mp4", 10 << 20, &missing, false, discordEmbed{}},
		{"jpeg", "p.jpg", "image/jpeg", 2 << 20, embedTestThumb(t, 320, 240), true, discordEmbed{
			ImageURL: origin + "/s/tok/stream/p.jpg", ImageWidth: 1280, ImageHeight: 960}},
		{"png without thumb", "p.png", "image/png", 2 << 20, nil, true, discordEmbed{
			ImageURL: origin + "/s/tok/stream/p.png"}},
		{"image over 10MiB", "p.jpg", "image/jpeg", 10<<20 + 1, embedTestThumb(t, 320, 240), false, discordEmbed{}},
		{"pdf", "a.pdf", "application/pdf", 1 << 20, embedTestThumb(t, 320, 240), false, discordEmbed{}},
		{"no mime", "clip.mp4", "", 1 << 20, embedTestThumb(t, 320, 180), false, discordEmbed{}},
		{"escaped filename", "phim hay #1?.mp4", "video/mp4", 1 << 20, embedTestThumb(t, 320, 180), true, esc},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			item := database.File{Filename: tc.filename, Size: tc.size, ThumbPath: tc.thumb, CreatedAt: created}
			if tc.mime != "" {
				m := tc.mime
				item.MimeType = &m
			}
			got, ok := buildDiscordEmbed(item, token, origin, maxVideo)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			want := tc.want
			want.Title = tc.filename
			want.Description = "Size: " + formatBytes(tc.size) + " - Created: 2026-11-18 10:00:00"
			if got != want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestDiscordEmbedTemplate(t *testing.T) {
	render := func(e discordEmbed) string {
		var sb strings.Builder
		if err := discordEmbedTemplate.Execute(&sb, e); err != nil {
			t.Fatal(err)
		}
		return sb.String()
	}

	video := render(discordEmbed{
		Title: `a"b<c>.mp4`, ImageURL: "https://example.com/s/tok/thumb",
		VideoURL: "https://example.com/s/tok/stream/clip.mp4", VideoType: "video/mp4",
		VideoWidth: 1280, VideoHeight: 720,
	})
	for _, line := range []string{
		`<meta property="og:type" content="video.other">`,
		`<meta property="og:video" content="https://example.com/s/tok/stream/clip.mp4">`,
		`<meta property="og:video:type" content="video/mp4">`,
		`<meta property="og:video:width" content="1280">`,
		`<meta property="og:video:height" content="720">`,
		`<meta property="og:image" content="https://example.com/s/tok/thumb">`,
		`<meta property="og:title" content="a&#34;b&lt;c&gt;.mp4">`,
	} {
		if !strings.Contains(video, line) {
			t.Errorf("video output missing %s\n%s", line, video)
		}
	}
	if strings.Contains(video, "summary_large_image") {
		t.Error("video output must not contain summary_large_image")
	}

	img := render(discordEmbed{
		Title: "p.jpg", ImageURL: "https://example.com/s/tok/stream/p.jpg",
		ImageWidth: 1280, ImageHeight: 960,
	})
	for _, line := range []string{
		`<meta name="twitter:card" content="summary_large_image">`,
		`<meta property="og:image:width" content="1280">`,
		`<meta property="og:image:height" content="960">`,
	} {
		if !strings.Contains(img, line) {
			t.Errorf("image output missing %s\n%s", line, img)
		}
	}
	if strings.Contains(img, "og:video") {
		t.Error("image output must not contain og:video")
	}
}

func TestHandleGetSharedFileDiscordEmbed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dbPath := filepath.Join(t.TempDir(), "embed.db")
	t.Cleanup(func() {
		database.RWDB.Close()
		database.RODB.Close()
	})
	if err := database.InitDB("sqlite", dbPath, ""); err != nil {
		t.Fatalf("failed to initialize db: %v", err)
	}

	tmpl := template.Must(template.New("share.html").Parse("SHARE_PAGE"))
	template.Must(tmpl.New("share_login.html").Parse("LOGIN_PAGE"))
	r := gin.New()
	r.SetHTMLTemplate(tmpl)
	h := &Handler{cfg: &config.Config{DiscordEmbedMaxVideoMB: 80}}
	r.GET("/s/:token", h.handleGetSharedFile)

	rows := []struct {
		token, filename, mime string
		size                  int64
		password              *string
	}{
		{"vid", "clip.mp4", "video/mp4", 40 << 20, nil},
		{"big", "big.mp4", "video/mp4", 81 << 20, nil},
		{"mkv", "a.mkv", "video/x-matroska", 10 << 20, nil},
		{"pw", "clip.mp4", "video/mp4", 10 << 20, func() *string { s := "x"; return &s }()},
		{"esc", `a"b<c>.mp4`, "video/mp4", 10 << 20, nil},
	}
	for _, row := range rows {
		_, err := database.DB.Exec("INSERT INTO files (message_id, filename, path, size, mime_type, share_token, thumb_path, share_password) VALUES (1, ?, ?, ?, ?, ?, ?, ?)",
			row.filename, "/"+row.token, row.size, row.mime, row.token, *embedTestThumb(t, 320, 180), row.password)
		if err != nil {
			t.Fatal(err)
		}
	}

	const discord = "Mozilla/5.0 (compatible; Discordbot/2.0; +https://discordapp.com)"
	const chrome = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/130.0"
	tests := []struct {
		name, token, ua string
		exact           string
		contains        string
		notContains     string
	}{
		{"discord video", "vid", discord, "", `<meta property="og:video" content="https://example.com/s/vid/stream/clip.mp4">`, "SHARE_PAGE"},
		{"browser unchanged", "vid", chrome, "SHARE_PAGE", "", ""},
		{"video over limit", "big", discord, "", "SHARE_PAGE", "og:video"},
		{"mkv", "mkv", discord, "", "SHARE_PAGE", ""},
		{"password protected", "pw", discord, "", "LOGIN_PAGE", "/stream"},
		{"escaped title", "esc", discord, "", `a&#34;b&lt;c&gt;.mp4`, `a"b<c>`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/s/"+tc.token, nil)
			req.Header.Set("User-Agent", tc.ua)
			req.Header.Set("X-Forwarded-Proto", "https")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			body := w.Body.String()
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			if tc.exact != "" && body != tc.exact {
				t.Errorf("body = %q, want %q", body, tc.exact)
			}
			if tc.contains != "" && !strings.Contains(body, tc.contains) {
				t.Errorf("body missing %q\n%s", tc.contains, body)
			}
			if tc.notContains != "" && strings.Contains(body, tc.notContains) {
				t.Errorf("body must not contain %q\n%s", tc.notContains, body)
			}
		})
	}
}
