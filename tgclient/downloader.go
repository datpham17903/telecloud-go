package tgclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"telecloud/config"
	"telecloud/database"

	"github.com/gotd/td/tg"
)

var (
	locationCache = make(map[int]*cachedLocation)
	cacheMutex    sync.RWMutex

	// Download progress tracking
	downloadProgress   = make(map[int]*DownloadProgress)
	downloadProgressMu sync.RWMutex
)

type DownloadProgress struct {
	Status    string
	Percent   int
	Message   string
	TotalSize int64
}

func SetDownloadProgress(fileID int, status DownloadProgress) {
	downloadProgressMu.Lock()
	defer downloadProgressMu.Unlock()
	downloadProgress[fileID] = &status
}

func GetDownloadProgress(fileID int) *DownloadProgress {
	downloadProgressMu.RLock()
	defer downloadProgressMu.RUnlock()
	if s, ok := downloadProgress[fileID]; ok {
		return s
	}
	return nil
}

func ClearDownloadProgress(fileID int) {
	downloadProgressMu.Lock()
	defer downloadProgressMu.Unlock()
	delete(downloadProgress, fileID)
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

	// Check if this is a chunked file by looking for chunks in DB
	var item database.File
	err := database.DB.Get(&item, "SELECT id, is_chunked FROM files WHERE message_id = ? LIMIT 1", msgID)
	if err == nil && item.IsChunked {
		// This is a chunked file - serve merged, pass fileID for progress tracking
		return ServeMergedFile(c, w, item.ID, msgID, filename, size, cfg)
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

// ServeMergedFile downloads all chunks and serves the merged file
func ServeMergedFile(c *http.Request, w http.ResponseWriter, fileID int, msgID int, filename string, size int64, cfg *config.Config) error {
	// Use background context so download isn't canceled when client disconnects
	// The HTTP response will still be written to, just not tied to the request context
	ctx := context.Background()

	// Find the parent file by message_id and get its actual database ID
	var parentFile database.File
	err := database.DB.Get(&parentFile, "SELECT * FROM files WHERE message_id = ? LIMIT 1", msgID)
	if err != nil {
		return fmt.Errorf("file not found in database")
	}

	if !parentFile.IsChunked {
		return fmt.Errorf("file is not chunked")
	}

	// Get the actual parent ID (could be the file's own id if it's the parent, or the parent_id if it's a chunk)
	parentID := parentFile.ID
	if parentFile.ParentID != nil && *parentFile.ParentID != 0 {
		parentID = *parentFile.ParentID
	}

	// Get all chunks for this parent (only records where parent_id matches, excludes parent itself)
	var chunks []database.File
	err = database.DB.Select(&chunks, "SELECT * FROM files WHERE parent_id = ? ORDER BY chunk_index", parentID)
	if err != nil || len(chunks) == 0 {
		return fmt.Errorf("chunks not found for parent_id %d", parentID)
	}

	// Sort by chunk index
	sort.Slice(chunks, func(i, j int) bool {
		if chunks[i].ChunkIndex == nil || chunks[j].ChunkIndex == nil {
			return false
		}
		return *chunks[i].ChunkIndex < *chunks[j].ChunkIndex
	})

	// Get original size
	var originalSize int64
	for _, chunk := range chunks {
		if chunk.OriginalSize != nil {
			originalSize = *chunk.OriginalSize
			break
		}
	}
	if originalSize == 0 {
		originalSize = size
	}

	fmt.Printf("[MergeDownload] Merging %d chunks for %s (original size: %d bytes)\n", len(chunks), filename, originalSize)

	// Create temp file for merged output
	tempDir := cfg.TempDir
	mergedPath := filepath.Join(tempDir, fmt.Sprintf("merged_%d_%s", parentID, filename))

	// Download and merge chunks
	outFile, err := os.Create(mergedPath)
	if err != nil {
		return fmt.Errorf("failed to create merged file: %v", err)
	}

	for i, chunk := range chunks {
		if chunk.MessageID == nil {
			continue
		}

		fmt.Printf("[MergeDownload] Downloading chunk %d/%d (msgID: %d)\n", i+1, len(chunks), *chunk.MessageID)

		// Update progress
		percent := (i * 100) / len(chunks)
		SetDownloadProgress(parentID, DownloadProgress{
			Status:  "downloading",
			Percent: percent,
			Message: fmt.Sprintf("Downloading chunk %d/%d...", i+1, len(chunks)),
		})

		// Get chunk size from document metadata on Telegram
		chunkSize := chunk.Size
		if chunkSize <= 0 {
			// If chunk size is 0, we need to get it from Telegram
			chunkSize = getTelegramDocumentSize(ctx, *chunk.MessageID, cfg)
		}

		reader, err := GetTelegramFileReader(ctx, *chunk.MessageID, chunkSize, cfg)
		if err != nil {
			outFile.Close()
			os.Remove(mergedPath)
			return fmt.Errorf("failed to get chunk %d reader: %v", i, err)
		}

		written, err := io.Copy(outFile, reader)
		if err != nil {
			outFile.Close()
			os.Remove(mergedPath)
			return fmt.Errorf("failed to copy chunk %d: %v", i, err)
		}
		fmt.Printf("[MergeDownload] Chunk %d downloaded: %d bytes\n", i+1, written)
	}

	outFile.Close()

	// Update progress: serving
	SetDownloadProgress(parentID, DownloadProgress{
		Status:  "done",
		Percent: 100,
		Message: "Download complete!",
	})

	// Serve the merged file
	mergedFile, err := os.Open(mergedPath)
	if err != nil {
		os.Remove(mergedPath)
		return fmt.Errorf("failed to open merged file: %v", err)
	}
	defer mergedFile.Close()
	defer os.Remove(mergedPath) // Clean up after serving

	// Set proper headers
	w.Header().Set("Accept-Ranges", "none")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", originalSize))

	http.ServeContent(w, c, filename, time.Time{}, mergedFile)

	// Clear progress after a delay (allow SSE to send final status)
	go func() {
		time.Sleep(5 * time.Second)
		ClearDownloadProgress(parentID)
	}()

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

// getTelegramDocumentSize fetches the document size from Telegram for a given message
func getTelegramDocumentSize(ctx context.Context, msgID int, cfg *config.Config) int64 {
	api := Client.API()
	peer, err := resolveLogGroup(ctx, api, cfg.LogGroupID)
	if err != nil {
		fmt.Printf("[MergeDownload] Failed to resolve peer for size lookup: %v\n", err)
		return 0
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
			fmt.Printf("[MergeDownload] Failed to get messages for size: %v\n", err)
			return 0
		}
		if m, ok := res.(*tg.MessagesChannelMessages); ok {
			msgs = m.Messages
		}
	} else {
		res, err := api.MessagesGetMessages(ctx, []tg.InputMessageClass{&tg.InputMessageID{ID: msgID}})
		if err != nil {
			fmt.Printf("[MergeDownload] Failed to get messages for size: %v\n", err)
			return 0
		}
		if m, ok := res.(*tg.MessagesMessages); ok {
			msgs = m.Messages
		} else if m, ok := res.(*tg.MessagesMessagesSlice); ok {
			msgs = m.Messages
		}
	}

	if len(msgs) == 0 {
		fmt.Printf("[MergeDownload] No messages found for size lookup\n")
		return 0
	}

	msg, ok := msgs[0].(*tg.Message)
	if !ok || msg.Media == nil {
		return 0
	}

	docMedia, ok := msg.Media.(*tg.MessageMediaDocument)
	if !ok {
		return 0
	}

	doc, ok := docMedia.Document.(*tg.Document)
	if !ok {
		return 0
	}

	fmt.Printf("[MergeDownload] Document size for msgID %d: %d bytes\n", msgID, doc.Size)
	return doc.Size
}
