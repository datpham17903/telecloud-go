package api

import (
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"telecloud/database"
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
