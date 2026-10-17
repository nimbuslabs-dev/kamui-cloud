package config

import (
	"errors"
	"os"
)

type Config struct {
	Token string
	Port  string
}

func Load() (*Config, error) {
	token := os.Getenv("KAMUI_TOKEN")
	if token == "" {
		return nil, errors.New("KAMUI_TOKEN is required")
	}
	return &Config{
		Token: token,
		Port:  "8080",
	}, nil
}
