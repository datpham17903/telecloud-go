package api

import (
	"html/template"
	"image"
	_ "image/jpeg"
	"net/url"
	"os"
	"strings"

	"telecloud/database"
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
	if item.MimeType == nil {
		return discordEmbed{}, false
	}
	mime := strings.ToLower(*item.MimeType)
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
