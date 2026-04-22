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
	AdminPassword   string
	MaxUploadSizeMB int
	DatabasePath    string
	ThumbsDir       string
	LogGroupID      string
	Port            string
}

func Load() *Config {
	err := godotenv.Load()
	if err != nil && !os.IsNotExist(err) {
		log.Printf("Error loading .env file: %v", err)
	}

	apiID, _ := strconv.Atoi(os.Getenv("API_ID"))
	maxUploadSizeMB, _ := strconv.Atoi(getEnv("MAX_UPLOAD_SIZE_MB", "2048"))

	logGroupID := os.Getenv("LOG_GROUP_ID")

	return &Config{
		APIID:           apiID,
		APIHash:         os.Getenv("API_HASH"),
		AdminPassword:   getEnv("ADMIN_PASSWORD", "telecloud_secret"),
		MaxUploadSizeMB: maxUploadSizeMB,
		DatabasePath:    getEnv("DATABASE_PATH", "database.db"),
		ThumbsDir:       getEnv("THUMBS_DIR", "static/thumbs"),
		LogGroupID:      logGroupID,
		Port:            getEnv("PORT", "8091"),
	}
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
