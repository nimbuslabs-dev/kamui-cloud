package config

import (
	"errors"
	"os"
	"strings"
	"unicode"
)

type Config struct {
	Token               string
	Port                string
	WorkspaceManagerURL string
	WorkspaceDataFile   string
}

func Load() (*Config, error) {
	token := os.Getenv("KAMUI_TOKEN")
	if len(token) < 32 || strings.ContainsFunc(token, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return nil, errors.New("KAMUI_TOKEN must contain at least 32 bytes without whitespace or control characters")
	}
	return &Config{
		Token:               token,
		Port:                "8080",
		WorkspaceDataFile:   getEnvOrDefault("KAMUI_WORKSPACE_DATA_FILE", "workspaces.json"),
		WorkspaceManagerURL: getEnvOrDefault("WORKSPACE_MANAGER_URL", "http://workspace-manager:8082"),
	}, nil
}

func getEnvOrDefault(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}
