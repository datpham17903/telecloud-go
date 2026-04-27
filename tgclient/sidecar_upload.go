package tgclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// SidecarClient communicates with the pyrogram sidecar service for Premium speed
type SidecarClient struct {
	baseURL string
	client  *http.Client
}

type SidecarUploadResponse struct {
	Success   bool   `json:"success"`
	MessageID int64  `json:"message_id"`
	Filename  string `json:"file_name"`
	FileSize  int64  `json:"file_size"`
	UploadID  string `json:"upload_id"`
	Error     string `json:"error"`
}

type SidecarStatusResponse struct {
	Connected bool `json:"connected"`
	User      struct {
		ID        int64  `json:"id"`
		Name      string `json:"name"`
		Username  string `json:"username"`
		IsPremium bool   `json:"is_premium"`
	} `json:"user"`
	Error string `json:"error"`
}

// NewSidecarClient creates a new sidecar client
func NewSidecarClient(baseURL string) *SidecarClient {
	return &SidecarClient{
		baseURL: baseURL,
		client: &http.Client{
			Timeout: 24 * time.Hour, // Large timeout for big files
		},
	}
}

// CheckStatus checks if the sidecar is connected and has Premium
func (sc *SidecarClient) CheckStatus() (*SidecarStatusResponse, error) {
	resp, err := sc.client.Get(sc.baseURL + "/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var status SidecarStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, err
	}
	return &status, nil
}

// UploadFile uploads a file via the sidecar (user session with Premium speed)
func (sc *SidecarClient) UploadFile(filePath string, chatID int64) (*SidecarUploadResponse, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// Create multipart form
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// Add file
	part, err := writer.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		return nil, fmt.Errorf("failed to create form file: %w", err)
	}
	if _, err := io.Copy(part, file); err != nil {
		return nil, fmt.Errorf("failed to copy file: %w", err)
	}

	// Add chat_id
	writer.WriteField("chat_id", fmt.Sprintf("%d", chatID))

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("failed to close writer: %w", err)
	}

	// Send request
	req, err := http.NewRequest("POST", sc.baseURL+"/upload", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := sc.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result SidecarUploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}
