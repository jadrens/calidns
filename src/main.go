package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/miekg/dns"

	"dns-server/src/api"
	"dns-server/src/config"
	"dns-server/src/geo"
	"dns-server/src/handler"
	"dns-server/src/recorder"
	"dns-server/src/resolver"
)

func newDNSServers(addr4, addr6 string) []*dns.Server {
	var servers []*dns.Server
	if addr4 != "" {
		servers = append(servers,
			&dns.Server{Addr: addr4, Net: "udp"},
			&dns.Server{Addr: addr4, Net: "tcp"},
		)
	}
	if addr6 != "" {
		servers = append(servers,
			&dns.Server{Addr: addr6, Net: "udp"},
			&dns.Server{Addr: addr6, Net: "tcp"},
		)
	}
	return servers
}

func main() {
	configPath := flag.String("config", filepath.Join("local", "config.yaml"), "Path to YAML configuration file")
	flag.Parse()

	// Generate default config if missing.
	if _, err := os.Stat(*configPath); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(*configPath), 0755); err != nil {
			log.Fatalf("Failed to create config directory: %v", err)
		}
		if err := config.GenerateDefault(*configPath); err != nil {
			log.Fatalf("Failed to generate default config: %v", err)
		}
		log.Printf("Generated default config: %s", *configPath)
	}

	// Load configuration.
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	log.Printf("Loaded %d zones from config", len(cfg.Zones))

	// Initialize components.
	updateInterval, _ := time.ParseDuration(cfg.Server.GeoIPUpdateInterval)
	geoLookup, err := geo.NewLookuper(filepath.Join(filepath.Dir(*configPath), "geo_cache.db"), 7*24*time.Hour, cfg.Server.GeoIPAPIKey, filepath.Join(filepath.Dir(*configPath), "geoip.dat"), cfg.Server.EnableGeoIPMmap, cfg.Server.GeoIPUpdateURL, updateInterval)
	if err != nil {
		log.Fatalf("Failed to initialize geo lookuper: %v", err)
	}
	defer geoLookup.Close()

	recorderType := "postgres"
	recorderSource := cfg.Database.DSN()
	if strings.EqualFold(strings.TrimSpace(cfg.Database.Type), "sqlite") {
		recorderType = "sqlite"
		recorderSource = cfg.Database.SQLitePath
		if !filepath.IsAbs(recorderSource) {
			recorderSource = filepath.Join(filepath.Dir(*configPath), recorderSource)
		}
	}
	rec, err := recorder.New(recorderType, recorderSource, geoLookup)
	if err != nil {
		log.Fatalf("Failed to initialize recorder: %v", err)
	}
	defer rec.Close()
	if recorderType == "sqlite" {
		log.Printf("Recorder initialized (SQLite: %s)", recorderSource)
	} else {
		log.Printf("Recorder initialized (PostgreSQL: %s/%s)", cfg.Database.Host, cfg.Database.DBName)
	}

	resolverIns := resolver.New(cfg)
	h := handler.New(cfg, resolverIns, geoLookup, rec)

	// DNS server.
	dns.HandleFunc(".", h.ServeDNS)
	dnsServers := newDNSServers(cfg.Server.Listen, cfg.Server.ListenIPv6)

	// HTTP API server.
	var apiSrv *http.Server
	if cfg.Server.API.Enabled {
		apiHandler := api.NewServer(resolverIns, rec, geoLookup, cfg.Server.API.Tokens, cfg.Server.API.CORS, cfg, *configPath)
		apiSrv = &http.Server{
			Addr:    cfg.Server.API.Listen,
			Handler: apiHandler,
		}
	}

	// Slave mode: sync config from master on startup.
	if cfg.Cluster.Mode == "slave" && cfg.Cluster.Master != "" {
		if err := syncFromMaster(cfg, resolverIns, *configPath); err != nil {
			log.Printf("[cluster] slave sync failed: %v", err)
		}
	}

	// Graceful shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("Received signal %v, shutting down...", sig)
		cancel()
		for _, server := range dnsServers {
			_ = server.Shutdown()
		}
		if apiSrv != nil {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutdownCancel()
			_ = apiSrv.Shutdown(shutdownCtx)
		}
	}()

	// Start HTTP API server.
	if apiSrv != nil {
		go func() {
			log.Printf("API server listening on %s", cfg.Server.API.Listen)
			if err := apiSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("API server error: %v", err)
			}
		}()
	}

	// Start DNS servers.
	addrs := cfg.Server.Listen
	if cfg.Server.ListenIPv6 != "" {
		addrs += " (IPv6: " + cfg.Server.ListenIPv6 + ")"
	}
	log.Printf("Starting authoritative DNS server on %s (UDP/TCP)", addrs)
	fmt.Printf("Default TTL: %ds | Default record: %v | Default response: %s\n",
		cfg.Server.DefaultTTL, cfg.Server.DefaultRecord, cfg.Server.DefaultResponse)

	for _, server := range dnsServers {
		server := server
		go func() {
			if err := server.ListenAndServe(); err != nil {
				log.Printf("DNS server error (%s): %v", server.Net, err)
			}
		}()
	}

	// Wait for shutdown to complete.
	<-ctx.Done()
	log.Println("Server stopped")
}

// syncFromMaster fetches the full config from the master server and applies zones.
func syncFromMaster(cfg *config.Config, res *resolver.Resolver, configPath string) error {
	url := "https://" + cfg.Cluster.Master + "/api/config"
	log.Printf("[cluster] slave: syncing config from master %s", url)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	token := cfg.Server.API.FirstToken()
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetching config: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("master returned %d: %s", resp.StatusCode, string(body))
	}

	var masterCfg config.Config
	if err := json.NewDecoder(resp.Body).Decode(&masterCfg); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}

	log.Printf("[cluster] slave: received %d zones from master", len(masterCfg.Zones))

	// Apply zones to the resolver.
	for pattern, zc := range masterCfg.Zones {
		countries := make(map[string]resolver.RecordSet, len(zc.Countries))
		for cc, rs := range zc.Countries {
			countries[cc] = resolver.RecordSet{
				A:     rs.A,
				AAAA:  rs.AAAA,
				TXT:   rs.TXT,
				CNAME: rs.CNAME,
				MX:    rs.MX,
				NS:    rs.NS,
				SRV:   rs.SRV,
				CAA:   rs.CAA,
				PTR:   rs.PTR,
				SOA:   rs.SOA,
				Other: rs.Other,
			}
		}
		ze := resolver.ZoneEntry{
			Pattern:   pattern,
			Regex:     pattern,
			Countries: countries,
			TTL:       zc.TTL,
			Record:    zc.Record,
			FastOpen:  zc.FastOpen,
		}
		res.UpsertZone(pattern, ze)
	}

	// Sync only business-level server settings; preserve local listener/cluster/database settings.
	cfg.Server.DefaultTTL = masterCfg.Server.DefaultTTL
	cfg.Server.DefaultRecord = masterCfg.Server.DefaultRecord
	cfg.Server.DefaultResponse = masterCfg.Server.DefaultResponse
	cfg.Server.GeoIPAPIKey = masterCfg.Server.GeoIPAPIKey
	// Sync API tokens and CORS; keep local Listen address and enabled flag.
	cfg.Server.API.Tokens = masterCfg.Server.API.Tokens
	cfg.Server.API.CORS = masterCfg.Server.API.CORS
	cfg.Zones = res.DumpZones()

	// Save to local config file.
	if err := cfg.Save(configPath); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}

	log.Printf("[cluster] slave: config synced and saved to %s", configPath)
	return nil
}
