package main

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the user deployment configuration (~/.agent/config.yaml)
type Config struct {
	Model ConfigModel  `yaml:"model"`
	Rules []string     `yaml:"rules"` // L2 project rule files to inject into workers

	// WaitingTimeoutMinutes is how long a ticket may sit in waiting_user
	// before it is auto-failed. 0 = default 30.
	WaitingTimeoutMinutes int `yaml:"waiting_timeout_minutes"`
	// MaxAttempts is how many times a worker may be retried after death
	// before the ticket is failed. 0 = default 3.
	MaxAttempts int `yaml:"max_attempts"`
}

// ConfigModel holds model defaults
type ConfigModel struct {
	Router   string `yaml:"router"`
	Worker   string `yaml:"worker"`
	Subagent string `yaml:"subagent"`
}

// LoadConfig reads ~/.agent/config.yaml; missing file is not an error
func LoadConfig(homeDir string) *Config {
	cfg := &Config{}
	path := filepath.Join(homeDir, ".agent", "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg // empty config is valid
	}
	// best-effort parse; on error keep defaults
	_ = yaml.Unmarshal(data, cfg)
	return cfg
}
