package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"
)

// DefaultOllamaURL is the address probed for a local Ollama instance
// during zero-config first run.
const DefaultOllamaURL = "http://localhost:11434"

// FirstRunProxyPort is the proxy port used in first-run mode. It must not
// be 11434 because that port is taken by the local Ollama we just detected.
const FirstRunProxyPort = 11435

// DefaultProbeTimeout is how long the first-run probe waits for Ollama.
const DefaultProbeTimeout = 2 * time.Second

// FirstRunResult holds the generated config plus the values main needs
// to print the first-run banner.
type FirstRunResult struct {
	Config      *Config
	APIKey      string
	AdminToken  string
	OllamaFound bool
	OllamaURL   string
}

// randomHex returns nBytes of crypto/rand entropy hex-encoded
// (2*nBytes characters).
func randomHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// ProbeOllama reports whether an Ollama instance responds at baseURL
// within timeout. It performs a plain GET on /api/tags.
func ProbeOllama(baseURL string, timeout time.Duration) bool {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(baseURL + "/api/tags")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// GenerateFirstRun builds a default config for zero-config first run:
// it probes ollamaURL for a local Ollama instance, generates a random
// API key and admin token, and returns a validated config listening on
// FirstRunProxyPort. The caller is responsible for saving the config
// (SaveConfig) and printing the banner.
func GenerateFirstRun(ollamaURL string, probeTimeout time.Duration) (*FirstRunResult, error) {
	keySuffix, err := randomHex(12) // 24 hex chars
	if err != nil {
		return nil, fmt.Errorf("generate api key: %w", err)
	}
	apiKey := "sk-mesh-" + keySuffix

	adminToken, err := randomHex(8) // 16 hex chars
	if err != nil {
		return nil, fmt.Errorf("generate admin token: %w", err)
	}

	found := ProbeOllama(ollamaURL, probeTimeout)

	cfg := &Config{
		Proxy: ProxyConfig{Port: FirstRunProxyPort},
		Auth: AuthConfig{Enabled: true,
			AdminToken: adminToken,
			Keys: []KeyConfig{
				{Name: "default", Key: apiKey, RateLimit: 1000},
			},
		},
		Metrics: MetricsConfig{Enabled: true},
	}
	if found {
		cfg.Nodes = []NodeConfig{{Name: "local", URL: ollamaURL}}
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate generated config: %w", err)
	}

	return &FirstRunResult{
		Config:      cfg,
		APIKey:      apiKey,
		AdminToken:  adminToken,
		OllamaFound: found,
		OllamaURL:   ollamaURL,
	}, nil
}
