// Package coder provides shared Coder backend helpers for the aggregated API server.
package coder

import (
	"fmt"
	"net/url"
	"time"

	"github.com/coder/coder/v2/codersdk"
)

const defaultRequestTimeout = 30 * time.Second

// Config describes how to construct a Coder SDK client.
type Config struct {
	CoderURL       *url.URL
	SessionToken   string
	RequestTimeout time.Duration
}

// NewSDKClient creates a configured Coder SDK client from cfg.
func NewSDKClient(cfg Config) (*codersdk.Client, error) {
	if cfg.CoderURL == nil {
		return nil, fmt.Errorf("assertion failed: coder URL must not be nil")
	}
	if cfg.SessionToken == "" {
		return nil, fmt.Errorf("assertion failed: session token must not be empty")
	}

	requestTimeout := cfg.RequestTimeout
	switch {
	case requestTimeout < 0:
		return nil, fmt.Errorf("assertion failed: request timeout must not be negative")
	case requestTimeout == 0:
		requestTimeout = defaultRequestTimeout
	}

	coderURL := *cfg.CoderURL
	client := codersdk.New(&coderURL)
	if client == nil {
		return nil, fmt.Errorf("assertion failed: coder SDK client is nil after successful construction")
	}
	if client.HTTPClient == nil {
		return nil, fmt.Errorf("assertion failed: coder SDK HTTP client is nil after successful construction")
	}

	client.HTTPClient.Timeout = requestTimeout
	client.SetSessionToken(cfg.SessionToken)
	if client.SessionToken() == "" {
		return nil, fmt.Errorf("assertion failed: coder SDK session token is empty after successful configuration")
	}

	return client, nil
}
