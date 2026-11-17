package api

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"telecloud/database"

	"github.com/gin-gonic/gin"
)

// discordCFMaxCacheBytes is Cloudflare's free-plan cache object limit.
const discordCFMaxCacheBytes int64 = 512 << 20

var (
	discordWarmTTL        = 20 * time.Hour
	discordWarmWait       = 6 * time.Second
	discordWarmRetryAfter = 10 * time.Minute
	warmFetch             = defaultWarmFetch
	warmedFiles           sync.Map // file ID (int) -> time.Time of last successful warm
	warmFailures          sync.Map // file ID (int) -> time.Time of last failed warm
	warmFlights           sync.Map // file ID (int) -> *warmFlight
	warmCurrentURL        sync.Map // file ID (int) -> URL of the live share; stale warms record nothing
	warmSlots             = make(chan struct{}, 2)

	// Never follow redirects: the warm target is fixed by configuration.
	warmClient = &http.Client{
		Timeout:       5 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
)

type warmFlight struct {
	url  string
	done chan struct{}
	ok   bool
}

// defaultWarmFetch downloads url fully, discarding the body, so that
// Cloudflare caches it. It returns the number of bytes read.
func defaultWarmFetch(ctx context.Context, url string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := warmClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return io.Copy(io.Discard, resp.Body)
}

// discordEmbedOrigin returns the origin for embed URLs and whether cache warming
// applies. Warming runs only for requests that arrive through the configured
// Cloudflare hostname, and then every URL is built from configuration, never
// from request-controlled headers.
func (h *Handler) discordEmbedOrigin(c *gin.Context) (string, bool) {
	base := h.cfg.DiscordCFBaseURL
	if base == "" || c.GetHeader("Cf-Ray") == "" {
		return getRequestOrigin(c), false
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || !strings.EqualFold(u.Host, c.Request.Host) {
		return getRequestOrigin(c), false
	}
	return base, true
}

// discordThumbCard drops the video part of an embed, leaving a large thumbnail card.
func discordThumbCard(e discordEmbed) discordEmbed {
	e.ImageWidth, e.ImageHeight = e.VideoWidth, e.VideoHeight
	e.VideoURL, e.VideoType = "", ""
	e.VideoWidth, e.VideoHeight = 0, 0
	return e
}

// prepareDiscordVideoEmbed makes sure a video embed is already in Cloudflare's
// cache before Discord fetches it; otherwise it falls back to a thumbnail card
// so Discord never shows a blank embed.
func prepareDiscordVideoEmbed(item database.File, embed discordEmbed) discordEmbed {
	if embed.VideoURL == "" {
		return embed
	}
	if item.Size > discordCFMaxCacheBytes {
		// Cloudflare will not cache it; serve the plain video embed without warming.
		return embed
	}
	if t, ok := warmedFiles.Load(item.ID); ok && time.Since(t.(time.Time)) < discordWarmTTL {
		return embed
	}
	if t, ok := warmFailures.Load(item.ID); ok && time.Since(t.(time.Time)) < discordWarmRetryAfter {
		return discordThumbCard(embed)
	}

	warmCurrentURL.Store(item.ID, embed.VideoURL)
	f := startWarm(item.ID, embed.VideoURL, item.Size)
	if f == nil {
		return discordThumbCard(embed)
	}

	timer := time.NewTimer(discordWarmWait)
	defer timer.Stop()
	select {
	case <-f.done:
		if f.ok {
			return embed
		}
	case <-timer.C:
	}
	return discordThumbCard(embed)
}

// startWarm starts, or joins, the single warm flight for a file. A flight for a
// different (stale) URL is replaced. It returns nil when no slot is free.
func startWarm(id int, url string, size int64) *warmFlight {
	f := &warmFlight{url: url, done: make(chan struct{})}
	for {
		existing, loaded := warmFlights.LoadOrStore(id, f)
		if !loaded {
			break
		}
		old := existing.(*warmFlight)
		if old.url == url {
			return old
		}
		if warmFlights.CompareAndSwap(id, old, f) {
			break
		}
	}
	select {
	case warmSlots <- struct{}{}:
		go runWarm(f, id, size)
		return f
	default:
		close(f.done)
		warmFlights.CompareAndDelete(id, f)
		return nil
	}
}

// clearWarmState forgets everything known about a file's warm state, used when
// its share is created or revoked (a new share has a new token, hence a new URL).
func clearWarmState(id int) {
	warmedFiles.Delete(id)
	warmFailures.Delete(id)
	warmCurrentURL.Delete(id)
}

// warmShareInBackground starts warming Cloudflare's cache for a freshly created
// share link without waiting for it. Skipped when it cannot help.
func (h *Handler) warmShareInBackground(item database.File, token string) {
	clearWarmState(item.ID)
	if h.cfg.DiscordCFBaseURL == "" || item.IsFolder {
		return
	}
	if item.SharePassword != nil && *item.SharePassword != "" {
		return
	}
	switch resolveEmbedMime(item) {
	case "video/mp4", "video/webm", "video/quicktime":
	default:
		return
	}
	if item.Size <= 0 || item.Size > discordCFMaxCacheBytes {
		return
	}
	u := discordMediaURL(h.cfg.DiscordCFBaseURL, token, item.Filename)
	warmCurrentURL.Store(item.ID, u)
	startWarm(item.ID, u, item.Size)
}

func runWarm(f *warmFlight, id int, size int64) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[Discord] cache warm panic for file %d: %v", id, r)
			f.ok = false
		}
		if cur, _ := warmCurrentURL.Load(id); cur == f.url {
			if f.ok {
				warmedFiles.Store(id, time.Now())
			} else {
				warmFailures.Store(id, time.Now())
			}
		}
		close(f.done)
		warmFlights.CompareAndDelete(id, f)
		<-warmSlots
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	n, err := warmFetch(ctx, f.url)
	if err != nil {
		log.Printf("[Discord] cache warm failed for file %d: %v", id, err)
		return
	}
	f.ok = n == size
}
