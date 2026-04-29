package tgclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"telecloud/config"
	"github.com/gotd/td/tg"
	"telecloud/database"
)

var (
	locationCache = make(map[int]*cachedLocation)
	cacheMutex    sync.RWMutex
)

func init() {
	// Dọn dẹp location cache expired mỗi 30 phút
	go func() {
		ticker := time.NewTicker(30 * time.Minute)
		for range ticker.C {
			now := time.Now()
			cacheMutex.Lock()
			for k, v := range locationCache {
				if now.After(v.expiresAt) {
					delete(locationCache, k)
				}
			}
			cacheMutex.Unlock()
		}
	}()
}

type cachedLocation struct {
	loc       *tg.InputDocumentFileLocation
	expiresAt time.Time
}

type tgFileReader struct {
	ctx         context.Context
	api         *tg.Client
	loc         tg.InputFileLocationClass
	size        int64
	offset      int64
	chunkOffset int64
	chunkData   []byte
}

func (r *tgFileReader) Read(p []byte) (int, error) {
	if r.offset >= r.size {
		return 0, io.EOF
	}

	chunkSize := int64(512 * 1024)
	chunkStart := (r.offset / chunkSize) * chunkSize

	if r.chunkData == nil || r.chunkOffset != chunkStart {
		req := &tg.UploadGetFileRequest{
			Precise:  true,
			Location: r.loc,
			Offset:   chunkStart,
			Limit:    int(chunkSize),
		}

		res, err := r.api.UploadGetFile(r.ctx, req)
		if err != nil {
			return 0, err
		}

		switch result := res.(type) {
		case *tg.UploadFile:
			r.chunkData = result.Bytes
			r.chunkOffset = chunkStart
			if len(r.chunkData) == 0 {
				return 0, io.EOF
			}
		case *tg.UploadFileCDNRedirect:
			return 0, fmt.Errorf("CDN redirect not supported")
		default:
			return 0, fmt.Errorf("unexpected type %T", res)
		}
	}

	inChunkOffset := r.offset - r.chunkOffset
	if inChunkOffset >= int64(len(r.chunkData)) {
		return 0, io.EOF
	}

	n := copy(p, r.chunkData[inChunkOffset:])
	r.offset += int64(n)
	return n, nil
}

func (r *tgFileReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		r.offset = offset
	case io.SeekCurrent:
		r.offset += offset
	case io.SeekEnd:
		r.offset = r.size + offset
	}
	if r.offset < 0 {
		r.offset = 0
	}
	return r.offset, nil
}

func ServeTelegramFile(c *http.Request, w http.ResponseWriter, msgID int, filename string, size int64, cfg *config.Config) error {
	ctx := c.Context()
	
	// Add some logging for debugging Range requests
	if rangeHeader := c.Header.Get("Range"); rangeHeader != "" {
		fmt.Printf("[Stream] Range request for %s: %s\n", filename, rangeHeader)
	}

	reader, err := GetTelegramFileReader(ctx, msgID, size, cfg)
	if err != nil {
		return err
	}

	// Set Accept-Ranges explicitly just in case
	w.Header().Set("Accept-Ranges", "bytes")
	
	http.ServeContent(w, c, filename, time.Time{}, reader)
	return nil
}

func GetTelegramFileReader(ctx context.Context, msgID int, size int64, cfg *config.Config) (io.ReadSeeker, error) {
	// Check cache first
	cacheMutex.RLock()
	cached, ok := locationCache[msgID]
	cacheMutex.RUnlock()

	if ok && time.Now().Before(cached.expiresAt) {
		return &tgFileReader{
			ctx:  ctx,
			api:  Client.API(),
			loc:  cached.loc,
			size: size,
		}, nil
	}

	api := Client.API()
	peer, err := resolveLogGroup(ctx, api, cfg.LogGroupID)
	if err != nil {
		return nil, err
	}

	var msgs tg.MessageClassArray

	if channel, ok := peer.(*tg.InputPeerChannel); ok {
		res, err := api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{
				ChannelID:  channel.ChannelID,
				AccessHash: channel.AccessHash,
			},
			ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: msgID}},
		})
		if err != nil {
			return nil, err
		}
		if m, ok := res.(*tg.MessagesChannelMessages); ok {
			msgs = m.Messages
		}
	} else {
		res, err := api.MessagesGetMessages(ctx, []tg.InputMessageClass{&tg.InputMessageID{ID: msgID}})
		if err != nil {
			return nil, err
		}
		if m, ok := res.(*tg.MessagesMessages); ok {
			msgs = m.Messages
		} else if m, ok := res.(*tg.MessagesMessagesSlice); ok {
			msgs = m.Messages
		}
	}

	if len(msgs) == 0 {
		return nil, fmt.Errorf("message not found")
	}

	msg, ok := msgs[0].(*tg.Message)
	if !ok || msg.Media == nil {
		return nil, fmt.Errorf("message has no media")
	}

	docMedia, ok := msg.Media.(*tg.MessageMediaDocument)
	if !ok {
		return nil, fmt.Errorf("media is not a document")
	}

	doc, ok := docMedia.Document.(*tg.Document)
	if !ok {
		return nil, fmt.Errorf("document is empty")
	}

	loc := doc.AsInputDocumentFileLocation()
	
	// Cache the location for 1 hour
	cacheMutex.Lock()
	locationCache[msgID] = &cachedLocation{
		loc:       loc,
		expiresAt: time.Now().Add(1 * time.Hour),
	}
	cacheMutex.Unlock()

	reader := &tgFileReader{
		ctx:  ctx,
		api:  api,
		loc:  loc,
		size: size,
	}

	return reader, nil
}

// DownloadAndMergeChunkedFile downloads all chunks and merges them
func DownloadAndMergeChunkedFile(ctx context.Context, msgID int, totalChunks int, originalSize int64, filename string, outputPath string, cfg *config.Config) error {
	largeFileTempDir := cfg.LargeFileTempDir
	if largeFileTempDir == "" {
		largeFileTempDir = "/opt/telecloud-temp"
	}
	os.MkdirAll(largeFileTempDir, 0755)

	chunkDir := filepath.Join(largeFileTempDir, fmt.Sprintf("merge_%d", msgID))
	os.MkdirAll(chunkDir, 0755)
	defer os.RemoveAll(chunkDir)

	for i := 0; i < totalChunks; i++ {
		var chunkMsgID int
		err := database.DB.Get(&chunkMsgID, "SELECT message_id FROM files WHERE parent_id = (SELECT id FROM files WHERE message_id = ?) AND chunk_index = ?", msgID, i)
		if err != nil {
			return fmt.Errorf("failed to get chunk %d info: %w", i, err)
		}

		reader, err := GetTelegramFileReader(ctx, chunkMsgID, -1, cfg)
		if err != nil {
			return fmt.Errorf("failed to download chunk %d: %w", i, err)
		}

		chunkPath := filepath.Join(chunkDir, fmt.Sprintf("%s.chunk.%d", filename, i))
		outFile, err := os.Create(chunkPath)
		if err != nil {
			return fmt.Errorf("failed to create chunk file %d: %w", i, err)
		}

		_, err = io.Copy(outFile, reader)
		outFile.Close()
		reader.(io.Closer).Close()
		if err != nil {
			return fmt.Errorf("failed to save chunk %d: %w", i, err)
		}
	}

	outFile, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer outFile.Close()

	for i := 0; i < totalChunks; i++ {
		chunkPath := filepath.Join(chunkDir, fmt.Sprintf("%s.chunk.%d", filename, i))
		chunkFile, err := os.Open(chunkPath)
		if err != nil {
			return fmt.Errorf("failed to open chunk %d: %w", i, err)
		}

		_, err = io.Copy(outFile, chunkFile)
		chunkFile.Close()
		if err != nil {
			return fmt.Errorf("failed to write chunk %d to output: %w", i, err)
		}

		os.Remove(chunkPath)
	}

	return nil
}
