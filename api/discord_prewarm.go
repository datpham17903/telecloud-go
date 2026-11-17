package api

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"telecloud/database"

	"github.com/gin-gonic/gin"
)

// discordCFMaxCacheBytes is Cloudflare's free-plan cache object limit.
const discordCFMaxCacheBytes int64 = 512 << 20

var (
	discordWarmTTL  = 20 * time.Hour
	discordWarmWait = 6 * time.Second
	warmFetch       = defaultWarmFetch
	warmedURLs      sync.Map // video URL -> time.Time of last successful warm
	warmFlights     sync.Map // video URL -> *warmFlight
	warmSlots       = make(chan struct{}, 2)
)

type warmFlight struct {
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return io.Copy(io.Discard, resp.Body)
}

// discordThumbCard drops the video part of an embed, leaving a large thumbnail card.
func discordThumbCard(e discordEmbed) discordEmbed {
	e.ImageWidth, e.ImageHeight = e.VideoWidth, e.VideoHeight
	e.VideoURL, e.VideoType = "", ""
	e.VideoWidth, e.VideoHeight = 0, 0
	return e
}

// prepareDiscordVideoEmbed makes sure a video embed requested through Cloudflare
// is already in Cloudflare's cache before Discord fetches it; otherwise it falls
// back to a thumbnail card so Discord never shows a blank embed.
func prepareDiscordVideoEmbed(c *gin.Context, item database.File, embed discordEmbed) discordEmbed {
	if c.GetHeader("Cf-Ray") == "" || embed.VideoURL == "" {
		return embed
	}
	if item.Size > discordCFMaxCacheBytes {
		return discordThumbCard(embed)
	}
	url := embed.VideoURL
	if t, ok := warmedURLs.Load(url); ok && time.Since(t.(time.Time)) < discordWarmTTL {
		return embed
	}

	f := &warmFlight{done: make(chan struct{})}
	if existing, loaded := warmFlights.LoadOrStore(url, f); loaded {
		f = existing.(*warmFlight)
	} else {
		select {
		case warmSlots <- struct{}{}:
			go runWarm(f, url, item.Size)
		default:
			close(f.done)
			warmFlights.Delete(url)
			return discordThumbCard(embed)
		}
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

func runWarm(f *warmFlight, url string, size int64) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[Discord] cache warm panic for %s: %v", url, r)
			f.ok = false
		}
		if f.ok {
			warmedURLs.Store(url, time.Now())
		}
		close(f.done)
		warmFlights.Delete(url)
		<-warmSlots
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	n, err := warmFetch(ctx, url)
	if err != nil {
		log.Printf("[Discord] cache warm failed for %s: %v", url, err)
		return
	}
	f.ok = n == size
}
