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

	"calidns/external_api"
	"calidns/src/api"
	"calidns/src/config"
	"calidns/src/dnsdata"
	"calidns/src/geo"
	"calidns/src/handler"
	"calidns/src/recorder"
	"calidns/src/resolver"
)

func newDNSServers(addresses []string) []*dns.Server {
	var servers []*dns.Server
	for _, addr := range addresses {
		servers = append(servers,
			&dns.Server{Addr: addr, Net: "udp"},
			&dns.Server{Addr: addr, Net: "tcp"},
		)
	}
	return servers
}

func parseFlags() (*string, *string) {
	configPath := flag.String("config", "", "Path to YAML configuration file")
	dataDir := flag.String("data-dir", "", "Directory for databases and GeoIP data")
	flag.Parse()
	return configPath, dataDir
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	configPath, dataDir := parseFlags()

	// Default config and data directory if not provided.
	if *configPath == "" {
		executable, err := os.Executable()
		if err != nil {
			return fmt.Errorf("getting executable path: %w", err)
		}
		workdir, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("getting working directory: %w", err)
		}
		*configPath = defaultConfigPath(executable, workdir)
	}
	if *dataDir == "" {
		*dataDir = defaultDataDir(*configPath)
	}

	// Generate default config if missing.
	if _, err := os.Stat(*configPath); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(*configPath), 0755); err != nil {
			return fmt.Errorf("creating config directory: %w", err)
		}
		if err := config.GenerateDefault(*configPath); err != nil {
			return fmt.Errorf("generating default config: %w", err)
		}
		log.Printf("Generated default config: %s", *configPath)
	}

	// Load configuration.
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if err := os.MkdirAll(*dataDir, 0750); err != nil {
		return fmt.Errorf("creating data directory: %w", err)
	}
	dataPath := filepath.Join(*dataDir, "dns_data.db")
	dnsStore, err := dnsdata.Open(dataPath)
	if err != nil {
		return fmt.Errorf("initializing DNS data: %w", err)
	}
	defer dnsStore.Close()
	defaults, err := dnsStore.LoadDefaults()
	if err != nil {
		return fmt.Errorf("loading DNS defaults: %w", err)
	}
	cfg.Server.DefaultTTL = defaults.TTL
	cfg.Server.DefaultRecord = defaults.Record
	cfg.Server.DefaultResponse = defaults.Response
	cfg.Zones, err = dnsStore.LoadZones()
	if err != nil {
		return fmt.Errorf("loading DNS zones: %w", err)
	}
	log.Printf("Loaded %d zones from %s", len(cfg.Zones), dataPath)

	// Initialize components.
	updateInterval, _ := time.ParseDuration(cfg.Server.GeoIP.UpdateInterval)
	externalCfg, err := external_api.LoadConfig(*configPath)
	if err != nil {
		return fmt.Errorf("loading external API config: %w", err)
	}
	geoProvider := external_api.NewIP2Location(externalCfg.GeoIPAPIKey)
	geoLookup, err := geo.NewLookuper(filepath.Join(*dataDir, "geo_cache.db"), 7*24*time.Hour, geoProvider, filepath.Join(*dataDir, "geoip.dat"), cfg.Server.GeoIP.EnableMmap, cfg.Server.GeoIP.UpdateURL, updateInterval)
	if err != nil {
		return fmt.Errorf("initializing geo lookuper: %w", err)
	}
	defer geoLookup.Close()

	recorderType := "postgres"
	recorderSource := cfg.Database.DSN()
	if strings.EqualFold(strings.TrimSpace(cfg.Database.Type), "sqlite") {
		recorderType = "sqlite"
		recorderSource = cfg.Database.SQLitePath
		if !filepath.IsAbs(recorderSource) {
			recorderSource = filepath.Join(*dataDir, recorderSource)
		}
	}
	rec, err := recorder.New(recorderType, recorderSource, geoLookup)
	if err != nil {
		return fmt.Errorf("initializing recorder: %w", err)
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
	dnsServers := newDNSServers(cfg.Server.Listen)

	// HTTP API server.
	var apiSrv *http.Server
	var apiHandler *api.Server
	if cfg.Server.API.Enabled {
		apiHandler, err = api.NewServer(resolverIns, rec, geoLookup, cfg.Server.API.Tokens, cfg.Server.API.CORS, cfg, dnsStore)
		if err != nil {
			return fmt.Errorf("initializing API server: %w", err)
		}
		defer apiHandler.Close()
		apiSrv = &http.Server{
			Addr:    cfg.Server.API.Listen,
			Handler: apiHandler,
		}
	}

	// Slave mode: sync config from master on startup.
	if cfg.Cluster.Mode == "slave" && cfg.Cluster.Master != "" {
		if err := syncFromMaster(cfg, resolverIns, dnsStore); err != nil {
			log.Printf("[cluster] slave sync failed: %v", err)
		}
	}

	// Graceful shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, len(dnsServers)+1)

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(sigCh)
		sig := <-sigCh
		log.Printf("Received signal %v, shutting down...", sig)
		cancel()
	}()

	// Start HTTP API server.
	if apiSrv != nil {
		go func() {
			log.Printf("API server listening on %s", cfg.Server.API.Listen)
			if err := apiSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				serveErr <- fmt.Errorf("API server %s: %w", cfg.Server.API.Listen, err)
			}
		}()
	}

	// Start DNS servers.
	log.Printf("Starting authoritative DNS server on %s (UDP/TCP)", strings.Join(cfg.Server.Listen, ", "))
	fmt.Printf("Default TTL: %ds | Default record: %v | Default response: %s\n",
		cfg.Server.DefaultTTL, cfg.Server.DefaultRecord, cfg.Server.DefaultResponse)

	for _, server := range dnsServers {
		server := server
		go func() {
			if err := server.ListenAndServe(); err != nil {
				serveErr <- fmt.Errorf("DNS server %s %s: %w", server.Net, server.Addr, err)
			}
		}()
	}

	// A listener failure is fatal: keeping the process alive would make systemd
	// report a healthy service that is not actually serving DNS or the API.
	var listenerErr error
	select {
	case <-ctx.Done():
	case listenerErr = <-serveErr:
		cancel()
	}
	for _, server := range dnsServers {
		_ = server.Shutdown()
	}
	if apiSrv != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = apiSrv.Shutdown(shutdownCtx)
		shutdownCancel()
	}
	log.Println("Server stopped")
	return listenerErr
}

// `/etc` When executable under
// `/usr/bin`
// `/usr/sbin`
// `/usr/local/bin`
// `/usr/local/sbin`
// `/bin`, `/sbin`
// Otherwise, use working directory
func defaultConfigPath(executable, workdir string) string {
	switch filepath.Dir(executable) {
	case "/usr/bin", "/usr/sbin", "/usr/local/bin", "/usr/local/sbin", "/bin", "/sbin":
		return "/etc/calidns/config.yaml"
	default:
		return filepath.Join(workdir, "config.yaml")
	}
}

/**
 * Default data directory.
 * /var/lib/calidns
 */
func defaultDataDir(configPath string) string {
	if filepath.Clean(filepath.Dir(configPath)) == "/etc/calidns" {
		return "/var/lib/calidns"
	}
	return filepath.Dir(configPath)
}

// syncFromMaster fetches the full config from the master server and applies zones.
func syncFromMaster(cfg *config.Config, res *resolver.Resolver, store *dnsdata.Store) error {
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
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetching config: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("master returned %d: %s", resp.StatusCode, string(body))
	}

	var masterCfg config.SyncConfig
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&masterCfg); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}

	log.Printf("[cluster] slave: received %d zones from master", len(masterCfg.Zones))

	// Replace the complete set so deletions made while a slave was offline are
	// reconciled on its next startup.
	if err := res.ReplaceZones(masterCfg.Zones); err != nil {
		return fmt.Errorf("applying zones: %w", err)
	}

	// Sync only business-level server settings; preserve local listener/cluster/database settings.
	cfg.Server.DefaultTTL = masterCfg.Server.DefaultTTL
	cfg.Server.DefaultRecord = masterCfg.Server.DefaultRecord
	cfg.Server.DefaultResponse = masterCfg.Server.DefaultResponse
	res.UpdateDefaults(uint32(cfg.Server.DefaultTTL), cfg.Server.DefaultRecord)
	cfg.Zones = res.DumpZones()

	if err := store.SaveSnapshot(dnsdata.Defaults{TTL: cfg.Server.DefaultTTL, Record: cfg.Server.DefaultRecord, Response: cfg.Server.DefaultResponse}, cfg.Zones); err != nil {
		return err
	}
	log.Printf("[cluster] slave: DNS data synced")
	return nil
}
