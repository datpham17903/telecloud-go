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

	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/message/html"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
)

var (
	UploadTasks = make(map[string]*UploadStatus)
	taskMutex   sync.Mutex
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
}

func GetTask(taskID string) *UploadStatus {
	taskMutex.Lock()
	defer taskMutex.Unlock()
	if t, ok := UploadTasks[taskID]; ok {
		return t
	}
	return &UploadStatus{Status: "pending", Percent: 0}
}

func resolveLogGroup(ctx context.Context, api *tg.Client, logGroupIDStr string) (tg.InputPeerClass, error) {
	if logGroupIDStr == "me" || logGroupIDStr == "self" {
		return &tg.InputPeerSelf{}, nil
	}

	logGroupID, err := strconv.ParseInt(logGroupIDStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid LOG_GROUP_ID: %v", err)
	}

	if logGroupID < 0 {
		strID := strconv.FormatInt(logGroupID, 10)
		if strings.HasPrefix(strID, "-100") {
			channelID, _ := strconv.ParseInt(strID[4:], 10, 64)
			dialogs, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
				OffsetPeer: &tg.InputPeerEmpty{},
				Limit:      100,
			})
			if err == nil {
				switch d := dialogs.(type) {
				case *tg.MessagesDialogs:
					for _, chat := range d.Chats {
						if c, ok := chat.(*tg.Channel); ok && c.ID == channelID {
							return &tg.InputPeerChannel{
								ChannelID:  c.ID,
								AccessHash: c.AccessHash,
							}, nil
						}
					}
				case *tg.MessagesDialogsSlice:
					for _, chat := range d.Chats {
						if c, ok := chat.(*tg.Channel); ok && c.ID == channelID {
							return &tg.InputPeerChannel{
								ChannelID:  c.ID,
								AccessHash: c.AccessHash,
							}, nil
						}
					}
				}
			}
		} else {
			chatID := -logGroupID
			return &tg.InputPeerChat{ChatID: chatID}, nil
		}
	} else {
		return &tg.InputPeerUser{UserID: logGroupID}, nil
	}
	
	return nil, fmt.Errorf("could not resolve peer for ID %s", logGroupIDStr)
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

	fileInfo, err := os.Stat(filePath)
	var size int64 = 0
	if err == nil {
		size = fileInfo.Size()
	}

	localThumb := utils.CreateLocalThumbnail(filePath, mimeType)

	// Save to DB
	_, err = database.DB.Exec(
		"INSERT INTO files (message_id, filename, path, size, mime_type, is_folder, thumb_path) VALUES (?, ?, ?, ?, ?, 0, ?)",
		msgID, filename, path, size, mimeType, localThumb,
	)
	if err != nil {
		UpdateTask(taskID, "error", 0, "DB Error: "+err.Error())
		return
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
