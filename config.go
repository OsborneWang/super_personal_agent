package main

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the user deployment configuration.
// Global config lives at ~/.agent/config.yaml.
// Project config lives at <project>/.agent/config.yaml and takes precedence.
type Config struct {
	Model ConfigModel `yaml:"model"`
	Rules []string    `yaml:"rules"` // L2 project rule files to inject into workers

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

// loadConfigFile reads a single yaml file; missing file is not an error
func loadConfigFile(path string) *Config {
	cfg := &Config{}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	_ = yaml.Unmarshal(data, cfg)
	return cfg
}

// LoadGlobalConfig reads ~/.agent/config.yaml
func LoadGlobalConfig(homeDir string) *Config {
	return loadConfigFile(filepath.Join(homeDir, ".agent", "config.yaml"))
}

// LoadProjectConfig reads <project>/.agent/config.yaml; nil if project empty
func LoadProjectConfig(project string) *Config {
	if project == "" {
		return nil
	}
	return loadConfigFile(filepath.Join(project, ".agent", "config.yaml"))
}

// EffectiveConfig merges global + project config, project taking precedence.
// Rules are concatenated (global first, then project) rather than overridden,
// because both global and project constraints apply.
func EffectiveConfig(global, project *Config) *Config {
	out := &Config{}
	if global != nil {
		*out = *global
		out.Rules = append([]string{}, global.Rules...)
	}
	if project != nil {
		if project.Model.Router != "" {
			out.Model.Router = project.Model.Router
		}
		if project.Model.Worker != "" {
			out.Model.Worker = project.Model.Worker
		}
		if project.Model.Subagent != "" {
			out.Model.Subagent = project.Model.Subagent
		}
		if project.WaitingTimeoutMinutes > 0 {
			out.WaitingTimeoutMinutes = project.WaitingTimeoutMinutes
		}
		if project.MaxAttempts > 0 {
			out.MaxAttempts = project.MaxAttempts
		}
		out.Rules = append(out.Rules, project.Rules...)
	}
	return out
}
