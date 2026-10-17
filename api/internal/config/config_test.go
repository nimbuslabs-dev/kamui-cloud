package config

import (
	"testing"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name      string
		token     string
		wantError bool
	}{
		{
			name:      "empty token fails",
			token:     "",
			wantError: true,
		},
		{
			name:      "valid token succeeds",
			token:     "secret-token",
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KAMUI_TOKEN", tt.token)
			cfg, err := Load()
			if tt.wantError {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if cfg != nil {
					t.Fatalf("expected nil config, got %v", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg == nil || cfg.Token != tt.token {
				t.Fatalf("expected token %q, got %v", tt.token, cfg)
			}
			if cfg.Port != "8080" {
				t.Fatalf("expected port 8080, got %q", cfg.Port)
			}
		})
	}
}
