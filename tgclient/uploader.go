package tgclient

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"telecloud/config"
	"telecloud/database"
	"telecloud/utils"
	"telecloud/ws"

	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/message/html"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
)

var (
	UploadTasks = make(map[string]*UploadStatus)
	TaskCancels = make(map[string]context.CancelFunc)
	taskMutex   sync.Mutex

	resolvedPeer   tg.InputPeerClass
	resolvedPeerID string
	resolvedPeerMu sync.RWMutex
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
	defer taskMutex.Unlock()
	if cancel, ok := TaskCancels[taskID]; ok {
		cancel()
		delete(TaskCancels, taskID)
	}
	if task, ok := UploadTasks[taskID]; ok {
		task.Status = "error"
		task.Message = "Upload cancelled"
	}
}

func resolveLogGroup(ctx context.Context, api *tg.Client, logGroupIDStr string) (tg.InputPeerClass, error) {
	resolvedPeerMu.RLock()
	if resolvedPeerID == logGroupIDStr && resolvedPeer != nil {
		p := resolvedPeer
		resolvedPeerMu.RUnlock()
		return p, nil
	}
	resolvedPeerMu.RUnlock()

	resolvedPeerMu.Lock()
	defer resolvedPeerMu.Unlock()

	// Double check
	if resolvedPeerID == logGroupIDStr && resolvedPeer != nil {
		return resolvedPeer, nil
	}

	var peer tg.InputPeerClass
	var err error

	if logGroupIDStr == "me" || logGroupIDStr == "self" {
		peer = &tg.InputPeerSelf{}
	} else {
		logGroupID, errParse := strconv.ParseInt(logGroupIDStr, 10, 64)
		if errParse != nil {
			return nil, fmt.Errorf("invalid LOG_GROUP_ID: %v", errParse)
		}

		if logGroupID < 0 {
			strID := strconv.FormatInt(logGroupID, 10)
			if strings.HasPrefix(strID, "-100") {
				channelID, _ := strconv.ParseInt(strID[4:], 10, 64)
				dialogs, errDlg := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
					OffsetPeer: &tg.InputPeerEmpty{},
					Limit:      100,
				})
				if errDlg == nil {
					switch d := dialogs.(type) {
					case *tg.MessagesDialogs:
						for _, chat := range d.Chats {
							if c, ok := chat.(*tg.Channel); ok && c.ID == channelID {
								peer = &tg.InputPeerChannel{
									ChannelID:  c.ID,
									AccessHash: c.AccessHash,
								}
								break
							}
						}
					case *tg.MessagesDialogsSlice:
						for _, chat := range d.Chats {
							if c, ok := chat.(*tg.Channel); ok && c.ID == channelID {
								peer = &tg.InputPeerChannel{
									ChannelID:  c.ID,
									AccessHash: c.AccessHash,
								}
								break
							}
						}
					}
				} else {
					err = errDlg
				}
			} else {
				peer = &tg.InputPeerChat{ChatID: -logGroupID}
			}
		} else {
			peer = &tg.InputPeerUser{UserID: logGroupID}
		}
	}

	if err != nil {
		return nil, err
	}
	if peer == nil {
		return nil, fmt.Errorf("could not resolve peer for ID %s", logGroupIDStr)
	}

	resolvedPeer = peer
	resolvedPeerID = logGroupIDStr
	return peer, nil
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

func ProcessCompleteUpload(ctx context.Context, filePath, filename, path, mimeType, taskID string, cfg *config.Config) {
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

	fileInfo, err := os.Stat(filePath)
	var totalSize int64 = 0
	if err == nil {
		totalSize = fileInfo.Size()
	}

	// Check if file needs chunking (> 2GB for non-Premium accounts)
	if totalSize > 2*1024*1024*1024 {
		ProcessChunkedUpload(ctx, filePath, filename, path, mimeType, taskID, cfg, totalSize)
		return
	}

	UpdateTask(taskID, "telegram", 0, "")

	api := Client.API()
	up := uploader.NewUploader(api).
		WithPartSize(uploader.MaximumPartSize).
		WithProgress(uploadProgress{taskID: taskID}).
		WithThreads(3)

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

	// Create caption
	caption := fmt.Sprintf("Path: %s\nFilename: %s", path, filename)

	docBuilder := message.UploadedDocument(file, html.String(nil, caption)).Filename(filename).MIME(mimeType)
	
	msgBuilder := sender.To(peer)

	res, err := msgBuilder.Media(ctx, docBuilder)
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

	localThumb := utils.CreateLocalThumbnail(filePath, mimeType, cfg.FFMPEGPath)

	// Save to DB
	_, err = database.DB.Exec(
		"INSERT INTO files (message_id, filename, path, size, mime_type, is_folder, thumb_path) VALUES (?, ?, ?, ?, ?, 0, ?)",
		msgID, filename, path, totalSize, mimeType, localThumb,
	)
	if err != nil {
		UpdateTask(taskID, "error", 0, "DB Error: "+err.Error())
		return
	}

	UpdateTask(taskID, "done", 100, "")
}

// ProcessChunkedUpload handles files larger than 4GB by splitting into chunks
func ProcessChunkedUpload(ctx context.Context, filePath, filename, path, mimeType, taskID string, cfg *config.Config, totalSize int64) {
	UpdateTask(taskID, "splitting", 0, "Splitting file into chunks...")

	// Split file into chunks
	chunkPaths, err := utils.SplitFile(filePath)
	if err != nil {
		UpdateTask(taskID, "error", 0, "Failed to split file: "+err.Error())
		return
	}

	totalChunks := len(chunkPaths)
	originalSize := totalSize

	// Update task with total chunks info
	UpdateTask(taskID, "splitting", 0, fmt.Sprintf("Split into %d chunks, uploading to Telegram...", totalChunks))

	api := Client.API()
	up := uploader.NewUploader(api).
		WithPartSize(uploader.MaximumPartSize).
		WithProgress(uploadProgress{taskID: taskID}).
		WithThreads(3)

	sender := message.NewSender(api)
	peer, err := resolveLogGroup(ctx, api, cfg.LogGroupID)
	if err != nil {
		utils.CleanupChunks(chunkPaths)
		UpdateTask(taskID, "error", 0, "Resolve peer error: "+err.Error())
		return
	}

	// Upload each chunk
	chunkMsgIDs := make([]int, totalChunks)
	for i, chunkPath := range chunkPaths {
		UpdateTask(taskID, "uploading_chunk", (i*100)/totalChunks, fmt.Sprintf("Uploading chunk %d/%d...", i+1, totalChunks))

		chunkFilename := fmt.Sprintf("%s.chunk.%d", filename, i)
		caption := fmt.Sprintf("Path: %s\nFilename: %s\nChunkIndex: %d\nTotalChunks: %d\nOriginalFilename: %s",
			path, chunkFilename, i, totalChunks, filename)

		file, err := up.FromPath(ctx, chunkPath)
		if err != nil {
			utils.CleanupChunks(chunkPaths)
			UpdateTask(taskID, "error", 0, fmt.Sprintf("Failed to upload chunk %d: %s", i, err.Error()))
			return
		}

		docBuilder := message.UploadedDocument(file, html.String(nil, caption)).Filename(chunkFilename).MIME(mimeType)
		msgBuilder := sender.To(peer)

		res, err := msgBuilder.Media(ctx, docBuilder)
		if err != nil {
			utils.CleanupChunks(chunkPaths)
			UpdateTask(taskID, "error", 0, fmt.Sprintf("Failed to send chunk %d: %s", i, err.Error()))
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

		chunkMsgIDs[i] = msgID

		// Clean up chunk file after successful upload
		os.Remove(chunkPath)

		UpdateTask(taskID, "uploading_chunk", ((i + 1) * 100) / totalChunks, fmt.Sprintf("Uploaded chunk %d/%d", i+1, totalChunks))
	}

	UpdateTask(taskID, "saving", 95, "Saving to database...")

	// Save parent file to DB (this is what user sees)
	// Note: message_id is set to chunkMsgIDs[0] to identify this as a chunked file
	// size is set to originalSize to show correct file size
	_, err = database.DB.Exec(
		"INSERT INTO files (message_id, filename, path, size, mime_type, is_folder, is_chunked, original_size) VALUES (?, ?, ?, ?, ?, 0, 1, ?)",
		chunkMsgIDs[0], filename, path, originalSize, mimeType, originalSize,
	)
	if err != nil {
		UpdateTask(taskID, "error", 0, "DB Error (parent): "+err.Error())
		return
	}

	parentID := 0
	err = database.DB.Get(&parentID, "SELECT last_insert_rowid()")
	if err != nil {
		UpdateTask(taskID, "error", 0, "Failed to get parent ID: "+err.Error())
		return
	}

	// Save chunk references to DB (hidden from user)
	for i, msgID := range chunkMsgIDs {
		_, err = database.DB.Exec(
			"INSERT INTO files (message_id, filename, path, size, mime_type, is_folder, is_chunked, parent_id, chunk_index, total_chunks) VALUES (?, ?, ?, ?, ?, 0, 1, ?, ?, ?)",
			msgID, fmt.Sprintf("%s.chunk.%d", filename, i), path, 0, mimeType, parentID, i, totalChunks,
		)
		if err != nil {
			// Non-fatal, chunks are uploaded, just might cause issues on delete
			fmt.Printf("Warning: Failed to save chunk %d to DB: %s\n", i, err.Error())
		}
	}

	UpdateTask(taskID, "done", 100, "")
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
