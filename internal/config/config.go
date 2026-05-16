package config

import (
	"fmt"
	"net/url"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Proxy   ProxyConfig   `yaml:"proxy"`
	Auth    AuthConfig    `yaml:"auth"`
	Nodes   []NodeConfig  `yaml:"nodes"`
	Routing RoutingConfig `yaml:"routing"`
	Metrics MetricsConfig `yaml:"metrics"`
	LiteLLM LiteLLMConfig `yaml:"litellm"`
}

type ProxyConfig struct {
	Port     int    `yaml:"port"`
	LogLevel string `yaml:"log_level"`
}

type AuthConfig struct {
	Enabled bool        `yaml:"enabled"`
	Keys    []KeyConfig `yaml:"keys"`
}

type KeyConfig struct {
	Name       string   `yaml:"name" json:"name"`
	Key        string   `yaml:"key" json:"key"`
	RateLimit  int      `yaml:"rate_limit" json:"rateLimit"`
	Models     []string `yaml:"models,omitempty" json:"models,omitempty"`
	ExpiresAt  string   `yaml:"expires_at,omitempty" json:"expiresAt,omitempty"`
}

type NodeConfig struct {
	Name        string `yaml:"name" json:"name"`
	URL         string `yaml:"url" json:"url"`
	GPUModel    string `yaml:"gpu_model" json:"gpu_model"`
	NvidiaIndex int    `yaml:"nvidia_index" json:"nvidia_index"`
}

type RoutingRule struct {
	ID         string `yaml:"id" json:"id"`
	Priority   int    `yaml:"priority" json:"priority"`
	Condition  string `yaml:"condition" json:"condition"`
	TargetNode string `yaml:"target_node" json:"targetNode"`
	Strategy   string `yaml:"strategy" json:"strategy"`
	Enabled    bool   `yaml:"enabled" json:"enabled"`
}

type RoutingConfig struct {
	Strategy       string        `yaml:"strategy" json:"strategy"`
	PollIntervalMs int           `yaml:"poll_interval_ms" json:"poll_interval_ms"`
	Fallback       string        `yaml:"fallback" json:"fallback"`
	Rules          []RoutingRule `yaml:"rules" json:"rules"`
}

type MetricsConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	Port    int  `yaml:"port" json:"port"`
}

type LiteLLMConfig struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	URL     string `yaml:"url" json:"url"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	return &cfg, nil
}

func SaveConfig(path string, cfg Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

func (c *Config) Validate() error {
	if c.Proxy.Port == 0 {
		c.Proxy.Port = 11434
	}
	if c.Proxy.LogLevel == "" {
		c.Proxy.LogLevel = "info"
	}
	if c.Routing.Strategy == "" {
		c.Routing.Strategy = "warm-first"
	}
	if c.Routing.PollIntervalMs == 0 {
		c.Routing.PollIntervalMs = 2000
	}
	if c.Routing.Fallback == "" {
		c.Routing.Fallback = "least-connections"
	}
	if c.Metrics.Port == 0 {
		c.Metrics.Port = 9090
	}

	if c.Auth.Enabled {
		seen := make(map[string]bool)
		for _, k := range c.Auth.Keys {
			if k.Name == "" || k.Key == "" {
				return fmt.Errorf("auth key must have name and key")
			}
			if seen[k.Name] {
				return fmt.Errorf("duplicate auth key name: %s", k.Name)
			}
			seen[k.Name] = true
		}
	}

	for i, n := range c.Nodes {
		if n.Name == "" || n.URL == "" {
			return fmt.Errorf("node %d must have name and url", i)
		}
		if _, err := url.Parse(n.URL); err != nil {
			return fmt.Errorf("invalid node URL %s: %w", n.URL, err)
		}
	}

	if c.LiteLLM.Enabled && c.LiteLLM.URL == "" {
		return fmt.Errorf("litellm URL required when enabled")
	}

	return nil
}
