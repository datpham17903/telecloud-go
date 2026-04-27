package utils

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// ChunkSize is 3.5GB to leave room for overhead and be safe under Telegram's 4GB limit
const ChunkSize = int64(3.5 * 1024 * 1024 * 1024)

// SplitFile splits a file into chunks of ChunkSize and returns chunk paths
func SplitFile(filePath string) ([]string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}

	totalSize := fileInfo.Size()
	chunkPaths := []string{}

	// Create temp dir for chunks
	tempDir := filepath.Dir(filePath)
	baseName := filepath.Base(filePath)

	chunkIndex := 0
	for offset := int64(0); offset < totalSize; offset += ChunkSize {
		chunkSize := ChunkSize
		if offset+ChunkSize > totalSize {
			chunkSize = totalSize - offset
		}

		chunkPath := filepath.Join(tempDir, fmt.Sprintf("%s.chunk.%d", baseName, chunkIndex))
		chunkPaths = append(chunkPaths, chunkPath)

		chunkFile, err := os.Create(chunkPath)
		if err != nil {
			// Cleanup on error
			for _, cp := range chunkPaths {
				os.Remove(cp)
			}
			return nil, err
		}

		written, err := io.CopyN(chunkFile, file, chunkSize)
		chunkFile.Close()
		if err != nil && err != io.EOF {
			for _, cp := range chunkPaths {
				os.Remove(cp)
			}
			return nil, err
		}
		if written != chunkSize {
			for _, cp := range chunkPaths {
				os.Remove(cp)
			}
			return nil, fmt.Errorf("expected to write %d bytes, wrote %d", chunkSize, written)
		}

		chunkIndex++
	}

	return chunkPaths, nil
}

// MergeChunks merges multiple chunk files into a single file
func MergeChunks(chunkPaths []string, outputPath string, deleteChunks bool) error {
	outFile, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer outFile.Close()

	var mu sync.Mutex
	var lastErr error

	for i, chunkPath := range chunkPaths {
		chunkFile, err := os.Open(chunkPath)
		if err != nil {
			lastErr = err
			continue
		}

		_, err = io.Copy(outFile, chunkFile)
		mu.Lock()
		if err != nil && err != io.EOF {
			lastErr = err
		}
		mu.Unlock()

		chunkFile.Close()

		if deleteChunks {
			os.Remove(chunkPath)
		}

		// Log progress for large files
		if (i+1)%10 == 0 {
			fmt.Printf("[Merge] Processed %d/%d chunks\n", i+1, len(chunkPaths))
		}
	}

	return lastErr
}

// GetTotalChunks returns the number of chunks for a given file size
func GetTotalChunks(fileSize int64) int {
	if fileSize <= 0 {
		return 0
	}
	chunks := int(fileSize / ChunkSize)
	if fileSize%ChunkSize != 0 {
		chunks++
	}
	return chunks
}

// CleanupChunks removes all chunk files from a list
func CleanupChunks(chunkPaths []string) {
	for _, cp := range chunkPaths {
		os.Remove(cp)
	}
}
