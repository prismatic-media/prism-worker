package main

import (
	"os"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		env           map[string]string
		files         map[string]string
		expectedValue func(cfg WorkerConfig) bool
		expectErr     bool
	}{
		{
			name: "Default configuration, no config file",
			args: []string{},
			env:  nil,
			files: map[string]string{},
			expectedValue: func(cfg WorkerConfig) bool {
				return cfg.FFmpegPath == "ffmpeg" && cfg.FFprobePath == "ffprobe" && cfg.ServerURL == ""
			},
			expectErr: false,
		},
		{
			name: "Config file loaded from default path",
			args: []string{},
			env:  nil,
			files: map[string]string{
				"worker_config.yaml": "server_url: https://prism.default\napi_key: default-key\nffmpeg_path: /usr/bin/ffmpeg",
			},
			expectedValue: func(cfg WorkerConfig) bool {
				return cfg.ServerURL == "https://prism.default" && cfg.APIKey == "default-key" && cfg.FFmpegPath == "/usr/bin/ffmpeg"
			},
			expectErr: false,
		},
		{
			name: "Environment variables override defaults",
			args: []string{},
			env: map[string]string{
				"PRISM_SERVER_URL": "https://prism.env",
				"PRISM_API_KEY":    "env-key",
				"PRISM_FFMPEG_PATH": "/env/ffmpeg",
			},
			files: map[string]string{},
			expectedValue: func(cfg WorkerConfig) bool {
				return cfg.ServerURL == "https://prism.env" && cfg.APIKey == "env-key" && cfg.FFmpegPath == "/env/ffmpeg"
			},
			expectErr: false,
		},
		{
			name: "CLI flags override environment variables and default config",
			args: []string{"-server-url", "https://prism.cli", "-api-key", "cli-key", "-ffmpeg-path", "/cli/ffmpeg"},
			env: map[string]string{
				"PRISM_SERVER_URL": "https://prism.env",
				"PRISM_API_KEY":    "env-key",
				"PRISM_FFMPEG_PATH": "/env/ffmpeg",
			},
			files: map[string]string{
				"worker_config.yaml": "server_url: https://prism.yaml\napi_key: yaml-key\nffmpeg_path: /yaml/ffmpeg",
			},
			expectedValue: func(cfg WorkerConfig) bool {
				return cfg.ServerURL == "https://prism.cli" && cfg.APIKey == "cli-key" && cfg.FFmpegPath == "/cli/ffmpeg"
			},
			expectErr: false,
		},
		{
			name: "Explicit configuration file requested but not found triggers error",
			args: []string{"-config", "nonexistent.yaml"},
			env:  nil,
			files: map[string]string{},
			expectedValue: func(cfg WorkerConfig) bool {
				return true
			},
			expectErr: true,
		},
		{
			name: "Explicit PRISM_CONFIG requested but not found triggers error",
			args: []string{},
			env: map[string]string{
				"PRISM_CONFIG": "nonexistent.yaml",
			},
			files: map[string]string{},
			expectedValue: func(cfg WorkerConfig) bool {
				return true
			},
			expectErr: true,
		},
		{
			name: "Ephemeral mode and registration token parsing",
			args: []string{},
			env: map[string]string{
				"PRISM_EPHEMERAL": "true",
				"PRISM_TOKEN":     "some-token",
			},
			files: map[string]string{},
			expectedValue: func(cfg WorkerConfig) bool {
				return cfg.Ephemeral == true && cfg.Token == "some-token"
			},
			expectErr: false,
		},
		{
			name: "Ephemeral mode and registration token from YAML",
			args: []string{},
			env:  nil,
			files: map[string]string{
				"worker_config.yaml": "ephemeral: true\ntoken: yaml-token",
			},
			expectedValue: func(cfg WorkerConfig) bool {
				return cfg.Ephemeral == true && cfg.Token == "yaml-token"
			},
			expectErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenvMock := func(key string) string {
				if tt.env == nil {
					return ""
				}
				return tt.env[key]
			}

			readFileMock := func(path string) ([]byte, error) {
				content, ok := tt.files[path]
				if !ok {
					return nil, os.ErrNotExist
				}
				return []byte(content), nil
			}

			cfg, err := LoadConfig(tt.args, getenvMock, readFileMock)
			if (err != nil) != tt.expectErr {
				t.Fatalf("expected error: %v, got: %v", tt.expectErr, err)
			}

			if err == nil {
				if tt.expectedValue != nil && !tt.expectedValue(cfg) {
					t.Errorf("config structure did not match expected values: %+v", cfg)
				}
			}
		})
	}
}
