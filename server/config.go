package main

import (
	"log"
	"os"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	Port           int          `toml:"port"`
	AllowedOrigins []string     `toml:"allowed_origins"`
	ViewerPolicy   ViewerPolicy `toml:"viewer_policy"`
}

type ViewerPolicy struct {
	MaxViewersPerNode int `toml:"max_viewers_per_node"`
}

func LoadConfig(path string) (Config, error) {
	var config Config
	// read config file
	fileData, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("Error reading file: %v", err)
	}

	// byte data => struct
	err = toml.Unmarshal(fileData, &config)
	if err != nil {
		log.Fatalf("Error unmarshalling TOML: %v", err)
	}
	return config, err
}
