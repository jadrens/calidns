package config

import (
	_ "embed"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// RecordSet holds DNS records for a single country entry.
type RecordSet struct {
	A     []string `yaml:"a,omitempty"`
	AAAA  []string `yaml:"aaaa,omitempty"`
	TXT   []string `yaml:"txt,omitempty"`
	CNAME []string `yaml:"cname,omitempty"`
	MX    []string `yaml:"mx,omitempty"`
	NS    []string `yaml:"ns,omitempty"`
	SRV   []string `yaml:"srv,omitempty"`
	CAA   []string `yaml:"caa,omitempty"`
	PTR   []string `yaml:"ptr,omitempty"`
	SOA   []string `yaml:"soa,omitempty"`
	Other []string `yaml:"other,omitempty"`
}

// ZoneConfig holds per-country record sets and zone-level settings.
type ZoneConfig struct {
	Countries map[string]*RecordSet `yaml:",inline"`
	TTL       *int                  `yaml:"ttl,omitempty"`
	Record    *bool                 `yaml:"record,omitempty"`
	FastOpen  *bool                 `yaml:"fast_open,omitempty"`
}

// APIConfig holds HTTP API server settings.
type APIConfig struct {
	Enabled bool       `yaml:"enabled"`
	Listen  string     `yaml:"listen"`
	Tokens  []string   `yaml:"tokens"`
	CORS    CORSConfig `yaml:"cors"`
}

// CORSConfig holds Cross-Origin Resource Sharing settings for the API server.
type CORSConfig struct {
	AllowOrigins     []string `yaml:"allow_origins"`
	AllowMethods     []string `yaml:"allow_methods"`
	AllowHeaders     []string `yaml:"allow_headers"`
	ExposeHeaders    []string `yaml:"expose_headers"`
	MaxAge           int      `yaml:"max_age"`
	AllowCredentials bool     `yaml:"allow_credentials"`
}

// ServerConfig holds top-level server settings.
type ServerConfig struct {
	Listen              string    `yaml:"listen"`
	ListenIPv6          string    `yaml:"listen_ipv6"`
	DefaultTTL          int       `yaml:"default_ttl"`
	DefaultRecord       bool      `yaml:"default_record"`
	DefaultResponse     string    `yaml:"default_response"`
	GeoIPAPIKey         string    `yaml:"geo_ip_api_key"`
	EnableGeoIPMmap     bool      `yaml:"enable_geoip_mmap"`
	GeoIPUpdateURL      string    `yaml:"geoip_update_url"`
	GeoIPUpdateInterval string    `yaml:"geoip_update_interval"`
	API                 APIConfig `yaml:"api"`
}

// ClusterConfig holds cluster mode and peer addresses.
type ClusterConfig struct {
	Mode   string   `yaml:"mode"`   // "master" or "slave"
	Slaves []string `yaml:"slaves"` // slave API domains (master mode)
	Master string   `yaml:"master"` // master API domain (slave mode)
}

// DBConfig selects the query recorder database and its connection settings.
type DBConfig struct {
	Type       string `yaml:"type"`
	Host       string `yaml:"host"`
	User       string `yaml:"user"`
	Password   string `yaml:"password"`
	DBName     string `yaml:"db_name"`
	SQLitePath string `yaml:"sqlite_path"`
}

// Config is the root configuration structure.
type Config struct {
	Server   ServerConfig           `yaml:"server"`
	Cluster  ClusterConfig          `yaml:"cluster"`
	Database DBConfig               `yaml:"database"`
	Zones    map[string]*ZoneConfig `yaml:"zones"`
}

// Load reads and parses a YAML config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	// First pass: unmarshal server + raw zones.
	var raw struct {
		Server   ServerConfig         `yaml:"server"`
		Cluster  ClusterConfig        `yaml:"cluster"`
		Database DBConfig             `yaml:"database"`
		Zones    map[string]yaml.Node `yaml:"zones"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	cfg := &Config{
		Server:   raw.Server,
		Cluster:  raw.Cluster,
		Database: raw.Database,
		Zones:    make(map[string]*ZoneConfig, len(raw.Zones)),
	}

	// Second pass: unmarshal each zone individually to separate
	// known country keys from the reserved ttl/record keys.
	reserved := map[string]bool{"ttl": true, "record": true, "fast_open": true}

	for pattern, node := range raw.Zones {
		zc := &ZoneConfig{
			Countries: make(map[string]*RecordSet),
		}

		// Unmarshal the zone as a generic map first.
		var zoneMap map[string]yaml.Node
		if err := node.Decode(&zoneMap); err != nil {
			return nil, fmt.Errorf("parsing zone %q: %w", pattern, err)
		}

		for key, val := range zoneMap {
			if reserved[key] {
				switch key {
				case "ttl":
					var t int
					if err := val.Decode(&t); err != nil {
						return nil, fmt.Errorf("parsing ttl in zone %q: %w", pattern, err)
					}
					zc.TTL = &t
				case "record":
					var r bool
					if err := val.Decode(&r); err != nil {
						return nil, fmt.Errorf("parsing record in zone %q: %w", pattern, err)
					}
					zc.Record = &r
				case "fast_open":
					var fo bool
					if err := val.Decode(&fo); err != nil {
						return nil, fmt.Errorf("parsing fast_open in zone %q: %w", pattern, err)
					}
					zc.FastOpen = &fo
				}
			} else {
				// Country code entry (or "default").
				var rs RecordSet
				if err := val.Decode(&rs); err != nil {
					return nil, fmt.Errorf("parsing country %q in zone %q: %w", key, pattern, err)
				}
				zc.Countries[key] = &rs
			}
		}

		cfg.Zones[pattern] = zc
	}

	// Set defaults.
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = ":53"
	}
	if cfg.Server.DefaultTTL <= 0 {
		cfg.Server.DefaultTTL = 300
	}
	if cfg.Server.DefaultResponse == "" {
		cfg.Server.DefaultResponse = "refuse"
	}
	if cfg.Server.GeoIPAPIKey == "" {
		cfg.Server.GeoIPAPIKey = "none"
	}
	if cfg.Server.GeoIPUpdateURL != "" {
		u, err := url.Parse(cfg.Server.GeoIPUpdateURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("geoip_update_url must be an absolute HTTP(S) URL")
		}
		if cfg.Server.GeoIPUpdateInterval == "" {
			cfg.Server.GeoIPUpdateInterval = "24h"
		}
		interval, err := time.ParseDuration(cfg.Server.GeoIPUpdateInterval)
		if err != nil || interval <= 0 {
			return nil, fmt.Errorf("geoip_update_interval must be a positive duration")
		}
	}
	if cfg.Server.API.Listen == "" {
		cfg.Server.API.Listen = ":3101"
	}
	if strings.EqualFold(strings.TrimSpace(cfg.Database.Type), "sqlite") && cfg.Database.SQLitePath == "" {
		cfg.Database.SQLitePath = "queries.db"
	}

	// CORS defaults: fully open.
	c := &cfg.Server.API.CORS
	if len(c.AllowOrigins) == 0 {
		c.AllowOrigins = []string{"*"}
	}
	if len(c.AllowMethods) == 0 {
		c.AllowMethods = []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "HEAD", "PATCH"}
	}
	if len(c.AllowHeaders) == 0 {
		c.AllowHeaders = []string{"Content-Type", "Authorization", "X-Requested-With", "Accept", "Origin"}
	}

	return cfg, nil
}

// HasRecordEnabled returns true if any zone has recording enabled or the
// global default enables it.
func (c *Config) HasRecordEnabled() bool {
	if c.Server.DefaultRecord {
		return true
	}
	for _, z := range c.Zones {
		if z.Record != nil && *z.Record {
			return true
		}
	}
	return false
}

//go:embed default_config.yaml
var defaultConfigYAML string

// Save writes the configuration back to a YAML file atomically
// (write to temp file, then rename).
func (c *Config) Save(path string) error {
	tmpPath := path + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("creating temp config: %w", err)
	}
	enc := yaml.NewEncoder(f)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("encoding config: %w", err)
	}
	enc.Close()
	f.Close()
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming config: %w", err)
	}
	return nil
}

// GenerateDefault writes a default configuration file to path.
// It will not overwrite an existing file.
func GenerateDefault(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return fmt.Errorf("creating default config: %w", err)
	}
	if _, err := f.WriteString(defaultConfigYAML); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("writing default config: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing default config: %w", err)
	}
	return nil
}

// HeaderValues returns pre-joined string values for HTTP CORS headers.
// This avoids calling strings.Join on every request.
func (c *CORSConfig) HeaderValues() (origin, methods, headers, expose string, maxAge int, credentials bool) {
	return strings.Join(c.AllowOrigins, ", "),
		strings.Join(c.AllowMethods, ", "),
		strings.Join(c.AllowHeaders, ", "),
		strings.Join(c.ExposeHeaders, ", "),
		c.MaxAge,
		c.AllowCredentials
}

// DSN returns the PostgreSQL connection string.
func (db *DBConfig) DSN() string {
	host, port := db.Host, "5432"
	if h, p, ok := strings.Cut(host, ":"); ok {
		host, port = h, p
	}
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		host, port, db.User, db.Password, db.DBName)
}

// FirstToken returns the first API token, or empty string if none configured.
func (a *APIConfig) FirstToken() string {
	if len(a.Tokens) == 0 {
		return ""
	}
	return a.Tokens[0]
}
