package utils

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	// Chunk sizes
	NormalChunkSize = int64(1900 * 1024 * 1024) // 1900MB for normal accounts
	PremiumChunkSize = int64(3800 * 1024 * 1024) // 3800MB for Premium accounts
)

// SplitFileStreaming splits a large file into smaller chunks
func SplitFileStreaming(filePath string, chunkSize int64, outputDir string) ([]string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	var chunks []string
	chunkIndex := 0
	buffer := make([]byte, chunkSize)

	for {
		bytesRead, err := file.Read(buffer)
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("failed to read file: %w", err)
		}

		if bytesRead == 0 {
			break
		}

		chunkFilename := fmt.Sprintf("%s.chunk.%d", filepath.Base(filePath), chunkIndex)
		chunkPath := filepath.Join(outputDir, chunkFilename)

		chunkFile, err := os.Create(chunkPath)
		if err != nil {
			return nil, fmt.Errorf("failed to create chunk file: %w", err)
		}

		_, err = chunkFile.Write(buffer[:bytesRead])
		chunkFile.Close()
		if err != nil {
			os.Remove(chunkPath)
			return nil, fmt.Errorf("failed to write chunk: %w", err)
		}

		chunks = append(chunks, chunkPath)
		chunkIndex++
	}

	return chunks, nil
}

// MergeChunks merges multiple chunk files into a single file
func MergeChunks(chunkPaths []string, outputPath string, cleanup bool) error {
	outFile, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer outFile.Close()

	for i, chunkPath := range chunkPaths {
		chunkFile, err := os.Open(chunkPath)
		if err != nil {
			return fmt.Errorf("failed to open chunk %d: %w", i, err)
		}

		_, err = io.Copy(outFile, chunkFile)
		chunkFile.Close()
		if err != nil {
			return fmt.Errorf("failed to write chunk %d to output: %w", i, err)
		}

		if cleanup {
			os.Remove(chunkPath)
		}
	}

	return nil
}

// GetTotalChunks calculates the number of chunks for a file
func GetTotalChunks(fileSize int64, chunkSize int64) int {
	if chunkSize <= 0 {
		chunkSize = NormalChunkSize
	}
	return int((fileSize + chunkSize - 1) / chunkSize)
}

// CleanupChunks removes all chunk files for a given task
func CleanupChunks(taskID string, outputDir string) error {
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), taskID+"_chunk.") {
			os.Remove(filepath.Join(outputDir, entry.Name()))
		}
	}
	return nil
}

// GetChunkPaths returns all chunk paths for a given task
func GetChunkPaths(taskID string, originalFilename string, outputDir string) []string {
	var chunks []string
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return chunks
	}

	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, taskID+"_"+originalFilename+".chunk.") {
			chunks = append(chunks, filepath.Join(outputDir, name))
		}
	}
	return chunks
}
