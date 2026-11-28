package config

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type Config struct {
	Token                string
	AdminPassword        string
	Port                 string
	CLIGatewayPort       int
	CAPath               string
	MDNSEnabled          bool
	WorkspaceManagerURL  string
	WorkspaceDataFile    string
	WorkspaceIdleTimeout time.Duration
	// S3 Storage Configuration
	S3Region          string
	S3Endpoint        string
	S3Bucket          string
	S3AccessKeyID     string
	S3SecretAccessKey string
	S3UsePathStyle    bool
	S3RootPrefix      string
}

func Load() (*Config, error) {
	token := os.Getenv("KAMUI_TOKEN")
	if len(token) < 32 || strings.ContainsFunc(token, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return nil, errors.New("KAMUI_TOKEN must contain at least 32 bytes without whitespace or control characters")
	}
	cliPort := 8443
	if rawPort := os.Getenv("KAMUI_CLI_PORT"); rawPort != "" {
		if parsed, err := strconv.Atoi(rawPort); err == nil && parsed > 0 && parsed <= 65535 {
			cliPort = parsed
		}
	}
	caPath := os.Getenv("KAMUI_CA_FILE")
	if caPath == "" {
		if tlsDir := os.Getenv("KAMUI_TLS_DIR"); tlsDir != "" {
			caPath = filepath.Join(tlsDir, "ca.pem")
		}
	}
	mdnsEnabled := os.Getenv("KAMUI_MDNS_ENABLED") == "true" || os.Getenv("KAMUI_CORE_EXECUTOR") == "" || os.Getenv("KAMUI_CORE_EXECUTOR") == "local"
	idleTimeout := 30 * time.Minute
	if raw := os.Getenv("KAMUI_WORKSPACE_IDLE_TIMEOUT"); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed >= 0 {
			idleTimeout = parsed
		}
	}
	return &Config{
		Token:                token,
		AdminPassword:        os.Getenv("KAMUI_ADMIN_PASSWORD"),
		Port:                 "8080",
		CLIGatewayPort:       cliPort,
		CAPath:               caPath,
		MDNSEnabled:          mdnsEnabled,
		WorkspaceDataFile:    getEnvOrDefault("KAMUI_WORKSPACE_DATA_FILE", "workspaces.json"),
		WorkspaceManagerURL:  getEnvOrDefault("WORKSPACE_MANAGER_URL", "http://workspace-manager:8082"),
		WorkspaceIdleTimeout: idleTimeout,
		S3Region:             os.Getenv("KAMUI_S3_REGION"),
		S3Endpoint:           os.Getenv("KAMUI_S3_ENDPOINT"),
		S3Bucket:             os.Getenv("KAMUI_S3_BUCKET"),
		S3AccessKeyID:        os.Getenv("KAMUI_S3_ACCESS_KEY_ID"),
		S3SecretAccessKey:    os.Getenv("KAMUI_S3_SECRET_ACCESS_KEY"),
		S3UsePathStyle:       os.Getenv("KAMUI_S3_USE_PATH_STYLE") == "true",
		S3RootPrefix:         os.Getenv("KAMUI_S3_ROOT_PREFIX"),
	}, nil
}

func getEnvOrDefault(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}
