package api

import (
	"context"
	"html/template"
	"image"
	_ "image/jpeg"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"telecloud/database"
	"telecloud/tgclient"
)

const discordEmbedMaxImageBytes int64 = 10 << 20

type discordEmbed struct {
	Title, Description, ImageURL string
	ImageWidth, ImageHeight      int
	VideoURL, VideoType          string
	VideoWidth, VideoHeight      int
}

var discordEmbedTemplate = template.Must(template.New("discord_embed").Parse(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>{{.Title}}</title>
<meta property="og:site_name" content="TeleCloud">
<meta property="og:title" content="{{.Title}}">
<meta property="og:description" content="{{.Description}}">
<meta property="og:image" content="{{.ImageURL}}">
{{- if .ImageWidth}}
<meta property="og:image:width" content="{{.ImageWidth}}">
<meta property="og:image:height" content="{{.ImageHeight}}">
{{- end}}
{{- if .VideoURL}}
<meta property="og:type" content="video.other">
<meta property="og:video" content="{{.VideoURL}}">
<meta property="og:video:type" content="{{.VideoType}}">
<meta property="og:video:width" content="{{.VideoWidth}}">
<meta property="og:video:height" content="{{.VideoHeight}}">
{{- else}}
<meta property="og:type" content="website">
<meta name="twitter:card" content="summary_large_image">
{{- end}}
</head>
<body></body>
</html>
`))

func isDiscordBot(userAgent string) bool {
	return strings.Contains(userAgent, "Discordbot")
}

// embedDimensions scales the thumbnail dimensions to a width of 1280.
func embedDimensions(thumbPath *string) (int, int) {
	if thumbPath == nil {
		return 0, 0
	}
	f, err := os.Open(*thumbPath)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return 0, 0
	}
	return 1280, (cfg.Height*1280 + cfg.Width/2) / cfg.Width
}

func buildDiscordEmbed(item database.File, token, origin string, maxVideoBytes int64) (discordEmbed, bool) {
	mime := resolveEmbedMime(item)
	mediaURL := origin + "/s/" + token + "/stream/" + url.PathEscape(item.Filename)
	embed := discordEmbed{
		Title:       item.Filename,
		Description: "Size: " + formatBytes(item.Size) + " - Created: " + item.CreatedAt.Format("2006-01-02 15:04:05"),
	}

	switch {
	case mime == "video/mp4" || mime == "video/webm" || mime == "video/quicktime":
		if item.Size > maxVideoBytes {
			return discordEmbed{}, false
		}
		w, h := embedDimensions(item.ThumbPath)
		if w == 0 {
			return discordEmbed{}, false
		}
		embed.ImageURL = origin + "/s/" + token + "/thumb"
		embed.VideoURL = mediaURL
		embed.VideoType = mime
		embed.VideoWidth, embed.VideoHeight = w, h
		return embed, true
	case discordImageMimes[mime]:
		if item.Size > discordEmbedMaxImageBytes {
			return discordEmbed{}, false
		}
		embed.ImageURL = mediaURL
		embed.ImageWidth, embed.ImageHeight = embedDimensions(item.ThumbPath)
		return embed, true
	}
	return discordEmbed{}, false
}

var discordImageMimes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "image/avif": true,
}

var embedExtMimes = map[string]string{
	".mp4": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime",
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp", ".avif": "image/avif",
}

// resolveEmbedMime returns the DB mime type, falling back to the file extension
// when the stored value is empty or the generic octet-stream.
func resolveEmbedMime(item database.File) string {
	mime := ""
	if item.MimeType != nil {
		mime = strings.ToLower(*item.MimeType)
	}
	if mime == "" || mime == "application/octet-stream" {
		if m, ok := embedExtMimes[strings.ToLower(filepath.Ext(item.Filename))]; ok {
			return m
		}
	}
	return mime
}

var (
	regenerateThumb        = tgclient.RegenerateFileThumbnail
	discordThumbWait       = 7 * time.Second
	discordThumbRetryAfter = 10 * time.Minute
	discordThumbSlots      = make(chan struct{}, 2) // caps concurrent regenerations
	thumbFlights           sync.Map                 // file ID (int) -> *thumbFlight
	thumbFailures          sync.Map                 // file ID (int) -> time.Time of last failure
)

type thumbFlight struct {
	done chan struct{}
	path *string
}

// ensureDiscordThumb regenerates a missing video thumbnail and waits briefly for it.
// Concurrent callers for the same file share one regeneration. Returns nil if the
// file is not eligible, recently failed, no slot is free, or the thumbnail is not
// ready in time.
func (h *Handler) ensureDiscordThumb(item database.File) *string {
	switch resolveEmbedMime(item) {
	case "video/mp4", "video/webm", "video/quicktime":
	default:
		return nil
	}
	if item.Size > int64(h.cfg.DiscordEmbedMaxVideoMB)<<20 {
		return nil
	}
	if w, _ := embedDimensions(item.ThumbPath); w != 0 {
		return nil
	}
	if t, ok := thumbFailures.Load(item.ID); ok && time.Since(t.(time.Time)) < discordThumbRetryAfter {
		return nil
	}

	f := &thumbFlight{done: make(chan struct{})}
	if existing, loaded := thumbFlights.LoadOrStore(item.ID, f); loaded {
		f = existing.(*thumbFlight)
	} else {
		select {
		case discordThumbSlots <- struct{}{}:
		default:
			close(f.done)
			thumbFlights.Delete(item.ID)
			return nil
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[Discord] thumbnail regeneration panic for file %d: %v", item.ID, r)
					f.path = nil
				}
				if f.path == nil {
					thumbFailures.Store(item.ID, time.Now())
				}
				close(f.done)
				thumbFlights.Delete(item.ID)
				<-discordThumbSlots
			}()
			if p, err := regenerateThumb(ctx, int64(item.ID), h.cfg); err == nil {
				f.path = p
			}
		}()
	}
	timer := time.NewTimer(discordThumbWait)
	defer timer.Stop()
	select {
	case <-f.done:
		return f.path
	case <-timer.C:
		return nil
	}
}
