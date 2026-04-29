package tgclient

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"time"

	"telecloud/config"
	"telecloud/database"
	"telecloud/utils"
	"telecloud/ws"

	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/message/html"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
)



const (
	// Chunk sizes
	NormalChunkSize = int64(1900 * 1024 * 1024)  // 1900MB for normal accounts
	PremiumChunkSize = int64(3800 * 1024 * 1024)  // 3800MB for Premium accounts
)

// GetChunkSize returns appropriate chunk size based on account type
func GetChunkSize(isPremium bool) int64 {
	if isPremium {
		return PremiumChunkSize
	}
	return NormalChunkSize
}

// ShouldUseChunking checks if file should be chunked
func ShouldUseChunking(fileSize int64, isPremium bool) bool {
	chunkSize := GetChunkSize(isPremium)
	return fileSize > chunkSize
}

// ProcessLargeFileUpload handles upload of files > chunk size
func ProcessLargeFileUpload(ctx context.Context, filePath, filename, path, mimeType, taskID string, cfg *config.Config, isPremium bool) error {
	ctx, cancel := context.WithCancel(ctx)
	taskMutex.Lock()
	TaskCancels[taskID] = cancel
	taskMutex.Unlock()

	defer func() {
		taskMutex.Lock()
		delete(TaskCancels, taskID)
		taskMutex.Unlock()
		cancel()
	}()

	// Use disk temp for large files
	largeFileTempDir := cfg.LargeFileTempDir
	if largeFileTempDir == "" {
		largeFileTempDir = "/opt/telecloud-temp"
	}
	os.MkdirAll(largeFileTempDir, 0755)

	chunkSize := GetChunkSize(isPremium)
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		UpdateTask(taskID, "error", 0, err.Error())
		return err
	}
	fileSize := fileInfo.Size()

	// Split file into chunks
	UpdateTask(taskID, "splitting", 0, "Splitting file into chunks...")
	chunks, err := utils.SplitFileStreaming(filePath, chunkSize, largeFileTempDir)
	if err != nil {
		UpdateTask(taskID, "error", 0, "Failed to split file: "+err.Error())
		return err
	}
	defer utils.CleanupChunks(taskID, largeFileTempDir)

	totalChunks := len(chunks)
	uploadedChunks := 0

	api := Client.API()
	sender := message.NewSender(api)
	peer, err := resolveLogGroup(ctx, api, cfg.LogGroupID)
	if err != nil {
		UpdateTask(taskID, "error", 0, "Resolve peer error: "+err.Error())
		return err
	}

	up := uploader.NewUploader(api).
		WithPartSize(uploader.MaximumPartSize).
		WithThreads(4)

	var parentMsgID int

	// Upload each chunk
	for i, chunkPath := range chunks {
		UpdateTask(taskID, "telegram", (uploadedChunks*100)/totalChunks, fmt.Sprintf("Uploading chunk %d/%d", i+1, totalChunks))

		file, err := up.FromPath(ctx, chunkPath)
		if err != nil {
			UpdateTask(taskID, "error", 0, fmt.Sprintf("Failed to read chunk %d: %v", i, err))
			return err
		}

		chunkFilename := fmt.Sprintf("%s.chunk.%d", filename, i)
		caption := fmt.Sprintf("Path: %s\nFilename: %s\nChunk: %d/%d\nParent: %t", path, filename, i+1, totalChunks, i > 0)
		docBuilder := message.UploadedDocument(file, html.String(nil, caption)).Filename(chunkFilename).MIME(mimeType)

		res, err := sender.To(peer).Media(ctx, docBuilder)
		if err != nil {
			UpdateTask(taskID, "error", 0, fmt.Sprintf("Failed to upload chunk %d: %v", i, err))
			return err
		}

		var msgID int
		if updReq, ok := res.(*tg.Updates); ok {
			for _, u := range updReq.Updates {
				if m, ok := u.(*tg.UpdateNewMessage); ok {
					if msg, ok := m.Message.(*tg.Message); ok {
						msgID = msg.ID
						break
					}
				} else if m, ok := u.(*tg.UpdateNewChannelMessage); ok {
					if msg, ok := m.Message.(*tg.Message); ok {
						msgID = msg.ID
						break
					}
				}
			}
		}

		if msgID <= 0 {
			UpdateTask(taskID, "error", 0, fmt.Sprintf("Failed to get message ID for chunk %d", i))
			return fmt.Errorf("failed to get message ID for chunk %d", i)
		}

		// First chunk is the parent file
		isParent := i == 0
		chunkIndex := i
		parentID := 0
		if !isParent && parentMsgID > 0 {
			parentID = parentMsgID
		}

		// Insert chunk info to DB
		_, err = database.DB.Exec(
			"INSERT INTO files (message_id, filename, path, size, mime_type, is_folder, thumb_path, is_chunked, parent_id, chunk_index, total_chunks, original_size) VALUES (?, ?, ?, ?, ?, 0, NULL, ?, ?, ?, ?, ?)",
			msgID, chunkFilename, path, fileInfo.Size(), mimeType, true, parentID, chunkIndex, totalChunks, fileSize,
		)
		if err != nil {
			UpdateTask(taskID, "error", 0, "DB error: "+err.Error())
			return err
		}

		if isParent {
			parentMsgID = msgID
		}

		uploadedChunks++
		os.Remove(chunkPath) // Clean up chunk after upload
	}

	UpdateTask(taskID, "done", 100, "")
	return nil
}

var (
	UploadTasks = make(map[string]*UploadStatus)
	TaskCancels = make(map[string]context.CancelFunc)
	taskMutex   sync.Mutex

	// Limit concurrent uploads to Telegram to prevent floodwait
	uploadSemaphore = make(chan struct{}, 3)
)

type UploadStatus struct {
	Status  string `json:"status"`
	Percent int    `json:"percent"`
	Message string `json:"message,omitempty"`
}

func UpdateTask(taskID string, status string, percent int, msg string) {
	taskMutex.Lock()
	defer taskMutex.Unlock()
	UploadTasks[taskID] = &UploadStatus{
		Status:  status,
		Percent: percent,
		Message: msg,
	}
	ws.BroadcastTaskUpdate(taskID, status, percent, msg)

	// Auto-cleanup: remove task from memory after 5 minutes once terminal
	if status == "done" || status == "error" {
		go func() {
			time.Sleep(5 * time.Minute)
			taskMutex.Lock()
			delete(UploadTasks, taskID)
			taskMutex.Unlock()
		}()
	}
}

func GetTask(taskID string) *UploadStatus {
	taskMutex.Lock()
	defer taskMutex.Unlock()
	if t, ok := UploadTasks[taskID]; ok {
		return t
	}
	return &UploadStatus{Status: "pending", Percent: 0}
}

func CancelTask(taskID string) {
	taskMutex.Lock()
	if cancel, ok := TaskCancels[taskID]; ok {
		cancel()
		delete(TaskCancels, taskID)
	}
	taskMutex.Unlock()

	// Gọi UpdateTask trong goroutine riêng để tránh deadlock (UpdateTask cũng lock taskMutex)
	// UpdateTask sẽ broadcast WS cho frontend và trigger cleanup goroutine 5 phút
	go UpdateTask(taskID, "error", 0, "upload_cancelled_waiting")
}


type uploadProgress struct {
	taskID string
}

func (p uploadProgress) Chunk(ctx context.Context, state uploader.ProgressState) error {
	if state.Total > 0 {
		percent := int(float64(state.Uploaded) / float64(state.Total) * 100)
		UpdateTask(p.taskID, "telegram", percent, "")
	}
	return nil
}

func ProcessCompleteUpload(ctx context.Context, filePath, filename, path, mimeType, taskID string, cfg *config.Config, overwrite bool) {
	ctx, cancel := context.WithCancel(ctx)
	taskMutex.Lock()
	TaskCancels[taskID] = cancel
	taskMutex.Unlock()

	defer func() {
		taskMutex.Lock()
		delete(TaskCancels, taskID)
		taskMutex.Unlock()
		cancel()
	}()

	UpdateTask(taskID, "telegram", 0, "waiting_slot")

	// Wait for a slot in the upload queue
	select {
	case uploadSemaphore <- struct{}{}:
		defer func() { <-uploadSemaphore }()
	case <-ctx.Done():
		UpdateTask(taskID, "error", 0, "upload_cancelled_waiting")
		return
	}

	UpdateTask(taskID, "telegram", 0, "")

	// Handle overwriting: single query instead of two
	var existingID int
	var existingMsgID *int
	var existingThumb *string
	if overwrite {
		database.DB.QueryRow("SELECT id, message_id, thumb_path FROM files WHERE path = ? AND filename = ? AND is_folder = 0", path, filename).Scan(&existingID, &existingMsgID, &existingThumb)
	}

	uniqueFilename := filename
	if !overwrite || existingID == 0 {
		uniqueFilename = database.GetUniqueFilename(database.DB, path, filename, false, 0)
	}

	api := Client.API()
	up := uploader.NewUploader(api).
		WithPartSize(uploader.MaximumPartSize).
		WithProgress(uploadProgress{taskID: taskID}).
		WithThreads(4)

	file, err := up.FromPath(ctx, filePath)
	if err != nil {
		UpdateTask(taskID, "error", 0, err.Error())
		return
	}

	sender := message.NewSender(api)
	peer, err := resolveLogGroup(ctx, api, cfg.LogGroupID)
	if err != nil {
		UpdateTask(taskID, "error", 0, "Resolve peer error: "+err.Error())
		return
	}

	caption := fmt.Sprintf("Path: %s\nFilename: %s", path, uniqueFilename)
	docBuilder := message.UploadedDocument(file, html.String(nil, caption)).Filename(uniqueFilename).MIME(mimeType)

	res, err := sender.To(peer).Media(ctx, docBuilder)
	if err != nil {
		UpdateTask(taskID, "error", 0, err.Error())
		return
	}

	var msgID int
	if updReq, ok := res.(*tg.Updates); ok {
		for _, u := range updReq.Updates {
			if m, ok := u.(*tg.UpdateNewMessage); ok {
				if msg, ok := m.Message.(*tg.Message); ok {
					msgID = msg.ID
					break
				}
			} else if m, ok := u.(*tg.UpdateNewChannelMessage); ok {
				if msg, ok := m.Message.(*tg.Message); ok {
					msgID = msg.ID
					break
				}
			}
		}
	}

	if msgID <= 0 {
		UpdateTask(taskID, "error", 0, "err_tg_msgid")
		return
	}

	fileInfo, err := os.Stat(filePath)
	var size int64 = 0
	if err == nil {
		size = fileInfo.Size()
	}

	// Insert to DB first (thumbnail generated async below)
	for i := 0; i < 5; i++ {
		_, err = database.DB.Exec(
			"INSERT INTO files (message_id, filename, path, size, mime_type, is_folder, thumb_path) VALUES (?, ?, ?, ?, ?, 0, NULL)",
			msgID, uniqueFilename, path, size, mimeType,
		)
		if err == nil {
			break
		}
		uniqueFilename = database.GetUniqueFilename(database.DB, path, filename, false, 0)
		time.Sleep(100 * time.Millisecond)
	}

	if err != nil {
		UpdateTask(taskID, "error", 0, "err_db_error: "+err.Error())
		return
	}

	// Overwrite cleanup
	if overwrite && existingID > 0 {
		database.DB.Exec("DELETE FROM files WHERE id = ?", existingID)
		if existingMsgID != nil {
			var count int
			database.DB.Get(&count, "SELECT COUNT(*) FROM files WHERE message_id = ?", *existingMsgID)
			if count == 0 {
				DeleteMessages(context.Background(), cfg, []int{*existingMsgID})
			}
		}
		if existingThumb != nil {
			var count int
			database.DB.Get(&count, "SELECT COUNT(*) FROM files WHERE thumb_path = ?", *existingThumb)
			if count == 0 {
				os.Remove(*existingThumb)
			}
		}
	}

	// Signal done to user immediately, then generate thumbnail during cooldown
	UpdateTask(taskID, "done", 100, "")

	// Cooldown before releasing the semaphore slot
	select {
	case <-time.After(1000 * time.Millisecond):
	case <-ctx.Done():
	}

	// Generate thumbnail from temp file (still exists at this point) and update DB
	localThumb := utils.CreateLocalThumbnail(filePath, mimeType, cfg.FFMPEGPath)
	if localThumb != nil {
		database.DB.Exec("UPDATE files SET thumb_path = ? WHERE message_id = ? AND path = ? AND filename = ?", *localThumb, msgID, path, uniqueFilename)
	}
}

// ProcessCompleteUploadSync is the synchronous version for the Upload API.
func ProcessCompleteUploadSync(ctx context.Context, filePath, filename, path, mimeType string, cfg *config.Config, overwrite bool) (fileID int64, finalName string, err error) {
	// Wait for a slot in the upload queue
	select {
	case uploadSemaphore <- struct{}{}:
		defer func() { <-uploadSemaphore }()
	case <-ctx.Done():
		return 0, "", fmt.Errorf("upload cancelled while waiting for queue")
	}

	api := Client.API()
	up := uploader.NewUploader(api).
		WithPartSize(uploader.MaximumPartSize).
		WithThreads(4)

	file, err := up.FromPath(ctx, filePath)
	if err != nil {
		return 0, "", fmt.Errorf("upload to telegram: %w", err)
	}

	sender := message.NewSender(api)
	peer, err := resolveLogGroup(ctx, api, cfg.LogGroupID)
	if err != nil {
		return 0, "", fmt.Errorf("resolve peer: %w", err)
	}

	// Handle overwriting: single query instead of two
	var existingID int
	var existingMsgID *int
	var existingThumb *string
	if overwrite {
		database.DB.QueryRow("SELECT id, message_id, thumb_path FROM files WHERE path = ? AND filename = ? AND is_folder = 0", path, filename).Scan(&existingID, &existingMsgID, &existingThumb)
	}

	uniqueFilename := filename
	if !overwrite || existingID == 0 {
		uniqueFilename = database.GetUniqueFilename(database.DB, path, filename, false, 0)
	}
	caption := fmt.Sprintf("Path: %s\nFilename: %s", path, uniqueFilename)
	docBuilder := message.UploadedDocument(file, html.String(nil, caption)).Filename(uniqueFilename).MIME(mimeType)

	res, err := sender.To(peer).Media(ctx, docBuilder)
	if err != nil {
		return 0, "", fmt.Errorf("send media: %w", err)
	}

	var msgID int
	if updReq, ok := res.(*tg.Updates); ok {
		for _, u := range updReq.Updates {
			if m, ok := u.(*tg.UpdateNewMessage); ok {
				if msg, ok := m.Message.(*tg.Message); ok {
					msgID = msg.ID
					break
				}
			} else if m, ok := u.(*tg.UpdateNewChannelMessage); ok {
				if msg, ok := m.Message.(*tg.Message); ok {
					msgID = msg.ID
					break
				}
			}
		}
	}

	if msgID <= 0 {
		return 0, "", fmt.Errorf("err_tg_msgid")
	}

	fileInfo, _ := os.Stat(filePath)
	var size int64
	if fileInfo != nil {
		size = fileInfo.Size()
	}

	// Thumbnail skipped for the sync API path (no temp file retention after return)
	var newID int64
	var dbErr error
	for i := 0; i < 5; i++ {
		var res sql.Result
		res, dbErr = database.DB.Exec(
			"INSERT INTO files (message_id, filename, path, size, mime_type, is_folder, thumb_path) VALUES (?, ?, ?, ?, ?, 0, NULL)",
			msgID, uniqueFilename, path, size, mimeType,
		)
		if dbErr == nil {
			newID, _ = res.LastInsertId()
			break
		}
		uniqueFilename = database.GetUniqueFilename(database.DB, path, filename, false, 0)
		time.Sleep(100 * time.Millisecond)
	}
	if dbErr != nil {
		return 0, "", fmt.Errorf("db insert: %w", dbErr)
	}

	// Clean up old file if overwriting
	if overwrite && existingID > 0 {
		database.DB.Exec("DELETE FROM files WHERE id = ?", existingID)
		if existingMsgID != nil {
			var count int
			database.DB.Get(&count, "SELECT COUNT(*) FROM files WHERE message_id = ?", *existingMsgID)
			if count == 0 {
				DeleteMessages(context.Background(), cfg, []int{*existingMsgID})
			}
		}
		if existingThumb != nil {
			var count int
			database.DB.Get(&count, "SELECT COUNT(*) FROM files WHERE thumb_path = ?", *existingThumb)
			if count == 0 {
				os.Remove(*existingThumb)
			}
		}
	}

	return newID, uniqueFilename, nil
}


func DeleteMessages(ctx context.Context, cfg *config.Config, msgIDs []int) error {
	if len(msgIDs) == 0 {
		return nil
	}
	api := Client.API()
	peer, err := resolveLogGroup(ctx, api, cfg.LogGroupID)
	if err != nil {
		return err
	}
	
	if channel, ok := peer.(*tg.InputPeerChannel); ok {
		_, err = api.ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash},
			ID:      msgIDs,
		})
		return err
	}
	
	_, err = api.MessagesDeleteMessages(ctx, &tg.MessagesDeleteMessagesRequest{
		Revoke: true,
		ID:     msgIDs,
	})
	return err
}
