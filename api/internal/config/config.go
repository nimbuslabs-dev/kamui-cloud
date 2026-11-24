package config

import (
	"errors"
	"os"
)

type Config struct {
	Token               string
	Port                string
	WorkspaceManagerURL string
}

func Load() (*Config, error) {
	token := os.Getenv("KAMUI_TOKEN")
	if token == "" {
		return nil, errors.New("KAMUI_TOKEN is required")
	}
	return &Config{
		Token:               token,
		Port:                "8080",
		WorkspaceManagerURL: getEnvOrDefault("WORKSPACE_MANAGER_URL", "http://workspace-manager:8082"),
	}, nil
}

func getEnvOrDefault(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}
