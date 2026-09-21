// Package external_api contains optional integrations with commercial services.
// The DNS server core depends only on geo.Provider; vendor-specific request and
// response formats live here so they can be replaced without changing core code.
package external_api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"calidns/src/geo"
	"gopkg.in/yaml.v3"
)

// Config configures the bundled external API implementation.
type Config struct {
	GeoIPAPIKey string `yaml:"geo_ip_api_key" json:"-"`
}

// LoadConfig reads only the external_api section. The core configuration
// package does not depend on vendor integration code.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var root struct {
		ExternalAPI Config `yaml:"external_api"`
	}
	if err := yaml.Unmarshal(data, &root); err != nil {
		return Config{}, err
	}
	return root.ExternalAPI, nil
}

// IP2Location implements geo.Provider using ip2location.io.
type IP2Location struct {
	apiKey string
	client *http.Client
}

// NewIP2Location returns the bundled provider. An empty or "none" key disables
// the external lookup and returns nil.
func NewIP2Location(apiKey string) geo.Provider {
	if apiKey == "" || apiKey == "none" {
		return nil
	}
	return &IP2Location{
		apiKey: apiKey,
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

type ip2locationResponse struct {
	CountryCode string `json:"country_code"`
	CityName    string `json:"city_name"`
	ASN         string `json:"asn"`
	AS          string `json:"as"`
}

type ip2locationError struct {
	Error struct {
		Code    int    `json:"error_code"`
		Message string `json:"error_message"`
	} `json:"error"`
}

// Lookup performs one vendor lookup. Caching remains the responsibility of
// the core geo.Lookuper.
func (p *IP2Location) Lookup(ctx context.Context, ip string) (geo.ProviderResult, error) {
	u := "https://api.ip2location.io/?key=" + url.QueryEscape(p.apiKey) +
		"&ip=" + url.QueryEscape(ip) + "&format=json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return geo.ProviderResult{}, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return geo.ProviderResult{}, fmt.Errorf("geo lookup: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return geo.ProviderResult{}, fmt.Errorf("geo read body: %w", err)
	}
	var apiErr ip2locationError
	if json.Unmarshal(body, &apiErr) == nil && apiErr.Error.Code != 0 {
		return geo.ProviderResult{}, fmt.Errorf("geo API error %d: %s", apiErr.Error.Code, apiErr.Error.Message)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return geo.ProviderResult{}, fmt.Errorf("geo API returned HTTP %d", resp.StatusCode)
	}
	var result ip2locationResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return geo.ProviderResult{}, fmt.Errorf("geo decode: %w", err)
	}
	if result.CountryCode == "" || result.CountryCode == "-" {
		return geo.ProviderResult{}, fmt.Errorf("geo API: empty country_code for %s", ip)
	}
	return geo.ProviderResult{
		CountryCode: stringPtr(result.CountryCode),
		SubLocation: stringPtr(result.CityName),
		ASN:         stringPtr(result.ASN),
		ASNName:     stringPtr(result.AS),
	}, nil
}

func stringPtr(value string) *string {
	if value == "" || value == "-" {
		return nil
	}
	return &value
}
