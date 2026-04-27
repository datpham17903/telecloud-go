package config

import (
	"log"
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

type Config struct {
	APIID           int
	APIHash         string
	MaxUploadSizeMB int
	DatabasePath    string
	ThumbsDir       string
	LogGroupID      string
	Port            string
	TempDir         string
	SmallFileTempDir string  // For files < 2GB (tmpfs)
	LargeFileTempDir string  // For files >= 2GB (disk)
	ProxyURL        string
	Version         string
	SessionFile     string
	FFMPEGPath      string
}

func Load() *Config {
	err := godotenv.Load()
	if err != nil && !os.IsNotExist(err) {
		log.Printf("Error loading .env file: %v", err)
	}

	apiID, _ := strconv.Atoi(os.Getenv("API_ID"))
	apiHash := os.Getenv("API_HASH")

	if apiID == 0 || apiHash == "" {
		log.Fatal("Error: API_ID and API_HASH must be set in .env. Please get them from https://my.telegram.org")
	}

	maxUploadSizeMB, _ := strconv.Atoi(getEnv("MAX_UPLOAD_SIZE_MB", "2048"))

	logGroupID := os.Getenv("LOG_GROUP_ID")

	// Small files (<2GB) use tmpfs for speed, large files use disk to save RAM
	smallTemp := getEnv("SMALL_FILE_TEMP_DIR", os.TempDir()+"/telecloud_temp_chunks")
	largeTemp := getEnv("LARGE_FILE_TEMP_DIR", "/opt/telecloud-temp")

	return &Config{
		APIID:           apiID,
		APIHash:         apiHash,
		MaxUploadSizeMB: maxUploadSizeMB,
		DatabasePath:    getEnv("DATABASE_PATH", "database.db"),
		ThumbsDir:       getEnv("THUMBS_DIR", "static/thumbs"),
		LogGroupID:      logGroupID,
		Port:            getEnv("PORT", "8091"),
		TempDir:         largeTemp, // Default to large file temp
		SmallFileTempDir: smallTemp,
		LargeFileTempDir: largeTemp,
		ProxyURL:        getEnv("PROXY_URL", ""),
		SessionFile:     getEnv("SESSION_FILE", "session.json"),
		FFMPEGPath:      getEnv("FFMPEG_PATH", "ffmpeg"),
	}
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
