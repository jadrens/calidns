package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"calidns/internal/dashboard"
	"calidns/src/config"
	"calidns/src/dnsdata"
	"calidns/src/geo"
	"calidns/src/recorder"
	"calidns/src/resolver"
)

// Server handles HTTP API requests.
type Server struct {
	resolver   *resolver.Resolver
	recorder   *recorder.Recorder
	geoLookup  *geo.Lookuper
	mux        *http.ServeMux
	tokens     map[string]struct{} // set of valid bearer tokens
	cfg        *config.Config      // server config (for persisting zone changes)
	dnsStore   *dnsdata.Store      // Web-API-managed DNS data
	dashboard  *dashboard.Handler  // built-in or downloaded dashboard assets
	httpClient *http.Client

	// Cluster sync.
	clusterMode string   // "master", "slave", or empty
	slaves      []string // slave API domains (master mode)
	masterURL   string   // master API URL (slave mode)
	firstToken  string   // first token for forwarding

	// Precomputed CORS header values.
	corsOrigin      string
	corsOrigins     []string
	corsMethods     string
	corsHeaders     string
	corsExpose      string
	corsMaxAge      int
	corsCredentials bool
}

// NewServer creates a new API server.
func NewServer(res *resolver.Resolver, rec *recorder.Recorder, geoLookup *geo.Lookuper, tokens []string, corsCfg config.CORSConfig, cfg *config.Config, dnsStore *dnsdata.Store) (*Server, error) {
	tokenSet := make(map[string]struct{}, len(tokens))
	for _, t := range tokens {
		if t = strings.TrimSpace(t); t != "" {
			tokenSet[t] = struct{}{}
		}
	}

	origin, methods, headers, expose, maxAge, creds := corsCfg.HeaderValues()
	var dashboardHandler *dashboard.Handler
	if cfg.Server.API.Dashboard.URL != "" {
		var err error
		var interval time.Duration
		if cfg.Server.API.Dashboard.URL != "default" {
			interval, _ = time.ParseDuration(cfg.Server.API.Dashboard.UpdateInterval)
		}
		dashboardHandler, err = dashboard.NewHandler(cfg.Server.API.Dashboard.URL, interval)
		if err != nil {
			return nil, fmt.Errorf("loading dashboard: %w", err)
		}
	}

	s := &Server{
		resolver:        res,
		recorder:        rec,
		geoLookup:       geoLookup,
		mux:             http.NewServeMux(),
		tokens:          tokenSet,
		cfg:             cfg,
		dnsStore:        dnsStore,
		dashboard:       dashboardHandler,
		httpClient:      &http.Client{Timeout: 10 * time.Second},
		clusterMode:     cfg.Cluster.Mode,
		slaves:          cfg.Cluster.Slaves,
		masterURL:       cfg.Cluster.Master,
		firstToken:      cfg.Server.API.FirstToken(),
		corsOrigin:      origin,
		corsOrigins:     append([]string(nil), corsCfg.AllowOrigins...),
		corsMethods:     methods,
		corsHeaders:     headers,
		corsExpose:      expose,
		corsMaxAge:      maxAge,
		corsCredentials: creds,
	}
	s.registerRoutes()
	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w, r)

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Health check is public.
	if r.URL.Path == "/api/health" && r.Method == http.MethodGet {
		s.mux.ServeHTTP(w, r)
		return
	}
	// The dashboard shell and static assets are public. API calls made by the
	// dashboard still pass through bearer-token authentication below.
	if s.cfg.Server.API.Dashboard.URL != "" &&
		(r.URL.Path == "/dashboard" || strings.HasPrefix(r.URL.Path, "/dashboard/")) {
		s.mux.ServeHTTP(w, r)
		return
	}

	// Log cluster-forwarded requests on slave.
	if s.clusterMode == "slave" && r.Header.Get("X-Cluster-Forward") == "master" {
		log.Printf("[cluster] received forwarded request from master: %s %s", r.Method, r.URL.RequestURI())
	}

	// Authenticate.
	if !s.authenticate(r) {
		writeError(w, http.StatusUnauthorized, "invalid or missing bearer token")
		return
	}

	s.mux.ServeHTTP(w, r)
}

// Close stops background API resources such as remote dashboard updates.
func (s *Server) Close() error {
	if s.dashboard != nil {
		return s.dashboard.Close()
	}
	return nil
}

func (s *Server) authenticate(r *http.Request) bool {
	// No tokens configured → allow all (dev mode).
	if len(s.tokens) == 0 {
		return true
	}

	auth := r.Header.Get("Authorization")
	if auth == "" {
		return false
	}

	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok {
		return false
	}

	_, valid := s.tokens[token]
	return valid
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("/api/zones", s.handleZones)
	s.mux.HandleFunc("/api/queries", s.handleQueries)
	s.mux.HandleFunc("/api/stats", s.handleStats)
	s.mux.HandleFunc("/api/health", s.handleHealth)
	s.mux.HandleFunc("/api/server", s.handleServer)
	s.mux.HandleFunc("/api/geo-cache", s.handleGeoCache)
	s.mux.HandleFunc("/api/edns", s.handleEDNS)
	s.mux.HandleFunc("/api/config", s.handleConfig)
	if s.cfg.Server.API.Dashboard.URL != "" {
		s.mux.HandleFunc("/dashboard", s.handleDashboardRoot)
		s.mux.Handle("/dashboard/", s.dashboard)
	}
}

func (s *Server) handleDashboardRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	http.Redirect(w, r, "/dashboard/", http.StatusTemporaryRedirect)
}

// --- Zone handlers ---

func (s *Server) handleZones(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		pattern := r.URL.Query().Get("pattern")
		if pattern != "" {
			s.getZone(w, r, pattern)
		} else {
			s.listZones(w, r)
		}
	case http.MethodPost, http.MethodPut:
		s.upsertZone(w, r)
	case http.MethodDelete:
		pattern := r.URL.Query().Get("pattern")
		if pattern == "" {
			writeError(w, http.StatusBadRequest, "pattern query parameter is required")
			return
		}
		country := r.URL.Query().Get("country")
		if country != "" {
			s.deleteCountry(w, r, pattern, country)
		} else {
			s.deleteZone(w, r, pattern)
		}
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) listZones(w http.ResponseWriter, r *http.Request) {
	zones := s.resolver.ListZones()
	if zones == nil {
		zones = []resolver.ZoneEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"zones": zones,
		"total": len(zones),
	})
}

func (s *Server) getZone(w http.ResponseWriter, r *http.Request, pattern string) {
	zone := s.resolver.GetZone(pattern)
	if zone == nil {
		writeError(w, http.StatusNotFound, "zone not found")
		return
	}
	writeJSON(w, http.StatusOK, zone)
}

func (s *Server) upsertZone(w http.ResponseWriter, r *http.Request) {
	// Read body for decoding and potential forwarding.
	bodyBytes, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read body: "+err.Error())
		return
	}

	var req struct {
		Pattern   string                        `json:"pattern"`
		Mode      string                        `json:"mode,omitempty"`
		Countries map[string]resolver.RecordSet `json:"countries"`
		TTL       *int                          `json:"ttl,omitempty"`
		Record    *bool                         `json:"record,omitempty"`
		FastOpen  *bool                         `json:"fast_open,omitempty"`
	}

	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// Trim whitespace from all IP records to prevent parse failures downstream.
	for cc, rs := range req.Countries {
		for i, ip := range rs.A {
			rs.A[i] = strings.TrimSpace(ip)
		}
		for i, ip := range rs.AAAA {
			rs.AAAA[i] = strings.TrimSpace(ip)
		}
		if err := resolver.ValidateRecordSet(rs); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("country %q: %v", cc, err))
			return
		}
		req.Countries[cc] = rs
	}

	if req.Pattern == "" {
		writeError(w, http.StatusBadRequest, "pattern is required")
		return
	}
	if req.TTL != nil && (*req.TTL <= 0 || uint64(*req.TTL) > uint64(^uint32(0))) {
		writeError(w, http.StatusBadRequest, "ttl must be between 1 and 4294967295")
		return
	}

	ze := resolver.ZoneEntry{
		Pattern:   req.Pattern,
		Regex:     req.Pattern,
		Mode:      req.Mode,
		Countries: req.Countries,
		TTL:       req.TTL,
		Record:    req.Record,
		FastOpen:  req.FastOpen,
	}

	previous := s.resolver.GetZone(req.Pattern)
	if !s.resolver.UpsertZone(req.Pattern, ze) {
		writeError(w, http.StatusBadRequest, "invalid zone mode or pattern")
		return
	}

	log.Printf("[API] zone upserted: %s", req.Pattern)
	if err := s.persistDNSData(); err != nil {
		s.restoreZone(req.Pattern, previous)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.forwardToSlaves(r.Method, r.URL.RequestURI(), bodyBytes)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "pattern": req.Pattern})
}

func (s *Server) deleteZone(w http.ResponseWriter, r *http.Request, pattern string) {
	previous := s.resolver.GetZone(pattern)
	if s.resolver.RemoveZone(pattern) {
		log.Printf("[API] zone deleted: %s", pattern)
		if err := s.persistDNSData(); err != nil {
			s.restoreZone(pattern, previous)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.forwardToSlaves(r.Method, r.URL.RequestURI(), nil)
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "pattern": pattern})
	} else {
		writeError(w, http.StatusNotFound, "zone not found")
	}
}

func (s *Server) deleteCountry(w http.ResponseWriter, r *http.Request, pattern, country string) {
	previous := s.resolver.GetZone(pattern)
	if s.resolver.RemoveCountry(pattern, country) {
		log.Printf("[API] country %q removed from zone %s", country, pattern)
		if err := s.persistDNSData(); err != nil {
			s.restoreZone(pattern, previous)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.forwardToSlaves(r.Method, r.URL.RequestURI(), nil)
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "pattern": pattern, "country": country})
	} else {
		writeError(w, http.StatusNotFound, "zone or country not found (default cannot be deleted)")
	}
}

// --- Query history handlers ---

func (s *Server) handleQueries(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.queryHistory(w, r)
	case http.MethodDelete:
		s.deleteQueries(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) queryHistory(w http.ResponseWriter, r *http.Request) {
	if s.recorder == nil {
		writeError(w, http.StatusServiceUnavailable, "recorder is not enabled")
		return
	}

	q := r.URL.Query()
	id, _ := strconv.ParseInt(q.Get("id"), 10, 64)
	domain := q.Get("domain")
	clientIP := q.Get("ip")
	subnet := q.Get("subnet")
	countryCode := q.Get("country_code")
	start := q.Get("start")
	end := q.Get("end")
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))

	if limit == 0 {
		limit = 50
	}

	result, err := s.recorder.QueryHistory(id, domain, clientIP, subnet, countryCode, start, end, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) deleteQueries(w http.ResponseWriter, r *http.Request) {
	if s.recorder == nil {
		writeError(w, http.StatusServiceUnavailable, "recorder is not enabled")
		return
	}

	q := r.URL.Query()
	id, _ := strconv.ParseInt(q.Get("id"), 10, 64)
	domain := q.Get("domain")
	clientIP := q.Get("ip")
	subnet := q.Get("subnet")
	countryCode := q.Get("country_code")
	start := q.Get("start")
	end := q.Get("end")

	n, err := s.recorder.DeleteQueries(id, domain, clientIP, subnet, countryCode, start, end)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	log.Printf("[API] deleted %d records", n)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "deleted",
		"deleted": n,
	})
}

// --- Stats ---

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	resp := map[string]interface{}{
		"zones": len(s.resolver.ListZones()),
	}

	if s.recorder != nil {
		stats, err := s.recorder.GetStats()
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "recorder statistics unavailable")
			return
		}
		resp["recorder"] = stats
	} else {
		resp["recorder"] = map[string]interface{}{"enabled": false}
	}

	writeJSON(w, http.StatusOK, resp)
}

// --- Health ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if s.recorder != nil {
		if err := s.recorder.Ping(); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "error", "error": "recorder database unavailable"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- Geo cache ---

func (s *Server) handleGeoCache(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listGeoCache(w, r)
	case http.MethodDelete:
		s.deleteGeoCache(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) listGeoCache(w http.ResponseWriter, r *http.Request) {
	countryCode := r.URL.Query().Get("country_code")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	if limit == 0 {
		limit = 50
	}

	entries, total, err := s.geoLookup.ListCache(countryCode, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"entries": entries,
		"total":   total,
	})
}

func (s *Server) deleteGeoCache(w http.ResponseWriter, r *http.Request) {
	subnet := r.URL.Query().Get("subnet")
	countryCode := r.URL.Query().Get("country_code")
	n, err := s.geoLookup.DeleteCache(subnet, countryCode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	msg := "all cached entries deleted"
	if subnet != "" {
		msg = fmt.Sprintf("deleted cache for %s", subnet)
	}
	if countryCode != "" {
		if subnet != "" {
			msg = fmt.Sprintf("deleted cache for %s and country_code=%s", subnet, countryCode)
		} else {
			msg = fmt.Sprintf("deleted cache for country_code=%s", countryCode)
		}
	}
	log.Printf("[API] geo-cache: %s (%d rows)", msg, n)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":       "deleted",
		"subnet":       subnet,
		"country_code": countryCode,
		"deleted":      n,
	})
}

// --- EDNS handlers ---

func (s *Server) handleEDNS(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.queryEDNS(w, r)
	case http.MethodDelete:
		s.deleteEDNS(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) queryEDNS(w http.ResponseWriter, r *http.Request) {
	if s.recorder == nil {
		writeError(w, http.StatusServiceUnavailable, "recorder is not enabled")
		return
	}

	q := r.URL.Query()
	id, _ := strconv.ParseInt(q.Get("id"), 10, 64)
	subnet := q.Get("subnet")
	countryCode := q.Get("country_code")
	nsid := q.Get("nsid")
	start := q.Get("start")
	end := q.Get("end")
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))

	if limit == 0 {
		limit = 50
	}

	result, err := s.recorder.QueryEDNS(id, subnet, countryCode, nsid, start, end, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) deleteEDNS(w http.ResponseWriter, r *http.Request) {
	if s.recorder == nil {
		writeError(w, http.StatusServiceUnavailable, "recorder is not enabled")
		return
	}

	q := r.URL.Query()
	id, _ := strconv.ParseInt(q.Get("id"), 10, 64)
	subnet := q.Get("subnet")
	countryCode := q.Get("country_code")
	nsid := q.Get("nsid")
	start := q.Get("start")
	end := q.Get("end")

	n, err := s.recorder.DeleteEDNS(id, subnet, countryCode, nsid, start, end)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	log.Printf("[API] deleted %d EDNS records", n)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "deleted",
		"deleted": n,
	})
}

// --- Server config ---

func (s *Server) handleServer(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.getServerConfig(w, r)
	case http.MethodPut:
		s.updateServerConfig(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) getServerConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"listen":           s.cfg.Server.Listen,
		"default_ttl":      s.cfg.Server.DefaultTTL,
		"default_response": s.cfg.Server.DefaultResponse,
		"default_record":   s.cfg.Server.DefaultRecord,
		"geoip": map[string]interface{}{
			"enable_mmap":     s.cfg.Server.GeoIP.EnableMmap,
			"update_url":      s.cfg.Server.GeoIP.UpdateURL,
			"update_interval": s.cfg.Server.GeoIP.UpdateInterval,
		},
		"dashboard": map[string]interface{}{
			"url":             s.dashboardURL(r),
			"update_interval": s.cfg.Server.API.Dashboard.UpdateInterval,
		},
	})
}

func (s *Server) dashboardURL(r *http.Request) string {
	if s.cfg.Server.API.Dashboard.URL == "" {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); forwarded == "http" || forwarded == "https" {
		scheme = forwarded
	}
	return scheme + "://" + r.Host + "/dashboard"
}

func (s *Server) updateServerConfig(w http.ResponseWriter, r *http.Request) {
	// Read body for decoding and potential forwarding.
	bodyBytes, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read body: "+err.Error())
		return
	}

	var req struct {
		DefaultTTL      *int    `json:"default_ttl,omitempty"`
		DefaultResponse *string `json:"default_response,omitempty"`
		DefaultRecord   *bool   `json:"default_record,omitempty"`
	}

	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	changed := false
	oldTTL := s.cfg.Server.DefaultTTL
	oldResponse := s.cfg.Server.DefaultResponse
	oldRecord := s.cfg.Server.DefaultRecord

	if req.DefaultTTL != nil {
		if *req.DefaultTTL <= 0 || uint64(*req.DefaultTTL) > uint64(^uint32(0)) {
			writeError(w, http.StatusBadRequest, "default_ttl must be between 1 and 4294967295")
			return
		}
		s.cfg.Server.DefaultTTL = *req.DefaultTTL
		changed = true
	}

	if req.DefaultResponse != nil {
		v := strings.ToLower(*req.DefaultResponse)
		if v != "refuse" && v != "nxdomain" && v != "servfail" {
			writeError(w, http.StatusBadRequest, "default_response must be one of: refuse, nxdomain, servfail")
			return
		}
		s.cfg.Server.DefaultResponse = v
		changed = true
	}

	if req.DefaultRecord != nil {
		s.cfg.Server.DefaultRecord = *req.DefaultRecord
		changed = true
	}

	if !changed {
		writeError(w, http.StatusBadRequest, "no valid fields to update")
		return
	}

	// Hot-reload resolver defaults (TTL and record are cached there).
	s.resolver.UpdateDefaults(uint32(s.cfg.Server.DefaultTTL), s.cfg.Server.DefaultRecord)

	if err := s.persistDNSData(); err != nil {
		s.cfg.Server.DefaultTTL = oldTTL
		s.cfg.Server.DefaultResponse = oldResponse
		s.cfg.Server.DefaultRecord = oldRecord
		s.resolver.UpdateDefaults(uint32(oldTTL), oldRecord)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.forwardToSlaves(r.Method, r.URL.RequestURI(), bodyBytes)

	log.Printf("[API] server config updated: ttl=%d response=%s record=%v",
		s.cfg.Server.DefaultTTL, s.cfg.Server.DefaultResponse, s.cfg.Server.DefaultRecord)

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- Config export (for slave sync) ---

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, config.SyncConfig{
		Server: config.SyncServerConfig{
			DefaultTTL:      s.cfg.Server.DefaultTTL,
			DefaultRecord:   s.cfg.Server.DefaultRecord,
			DefaultResponse: s.cfg.Server.DefaultResponse,
		},
		Zones: s.resolver.DumpZones(),
	})
}

// --- Cluster forwarding ---

// forwardToSlaves sends the same request to all slave servers (best-effort).
func (s *Server) forwardToSlaves(method, path string, body []byte) {
	if s.clusterMode != "master" || len(s.slaves) == 0 {
		return
	}

	log.Printf("[cluster] forwarding %s %s to %d slave(s): %v", method, path, len(s.slaves), s.slaves)

	var wg sync.WaitGroup
	for _, slave := range s.slaves {
		slave := slave
		wg.Add(1)
		go func() {
			defer wg.Done()
			url := "https://" + slave + path
			var req *http.Request
			var err error
			if body != nil {
				req, err = http.NewRequest(method, url, bytes.NewReader(body))
			} else {
				req, err = http.NewRequest(method, url, nil)
			}
			if err != nil {
				log.Printf("[cluster] failed to create request for %s: %v", slave, err)
				return
			}
			req.Header.Set("Authorization", "Bearer "+s.firstToken)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Cluster-Forward", "master")

			resp, err := s.httpClient.Do(req)
			if err != nil {
				log.Printf("[cluster] failed to forward to %s: %v", slave, err)
				return
			}
			resp.Body.Close()
			if resp.StatusCode >= 300 {
				log.Printf("[cluster] slave %s returned %d for %s %s", slave, resp.StatusCode, method, path)
			} else {
				log.Printf("[cluster] forwarded to %s OK (%s %s)", slave, method, path)
			}
		}()
	}
	wg.Wait()
}

// --- CORS ---

func (s *Server) setCORSHeaders(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	allowedOrigin := ""
	for _, candidate := range s.corsOrigins {
		if candidate == "*" {
			if s.corsCredentials && origin != "" {
				allowedOrigin = origin
			} else {
				allowedOrigin = "*"
			}
			break
		}
		if origin != "" && candidate == origin {
			allowedOrigin = origin
			break
		}
	}
	if allowedOrigin == "" && len(s.corsOrigins) <= 1 {
		allowedOrigin = s.corsOrigin
	}
	if allowedOrigin != "" {
		w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
		if allowedOrigin != "*" {
			w.Header().Add("Vary", "Origin")
		}
	}
	w.Header().Set("Access-Control-Allow-Methods", s.corsMethods)
	w.Header().Set("Access-Control-Allow-Headers", s.corsHeaders)
	if s.corsExpose != "" {
		w.Header().Set("Access-Control-Expose-Headers", s.corsExpose)
	}
	if s.corsMaxAge > 0 {
		w.Header().Set("Access-Control-Max-Age", strconv.Itoa(s.corsMaxAge))
	}
	if s.corsCredentials {
		w.Header().Set("Access-Control-Allow-Credentials", "true")
	}
}

// --- DNS data persistence ---

func (s *Server) persistDNSData() error {
	s.cfg.Zones = s.resolver.DumpZones()
	if s.dnsStore == nil {
		return nil
	}
	if err := s.dnsStore.SaveSnapshot(dnsdata.Defaults{
		TTL: s.cfg.Server.DefaultTTL, Record: s.cfg.Server.DefaultRecord,
		Response: s.cfg.Server.DefaultResponse,
	}, s.cfg.Zones); err != nil {
		return fmt.Errorf("saving DNS data: %w", err)
	}
	return nil
}

func (s *Server) restoreZone(pattern string, previous *resolver.ZoneEntry) {
	if previous == nil {
		s.resolver.RemoveZone(pattern)
	} else {
		s.resolver.UpsertZone(pattern, *previous)
	}
	s.cfg.Zones = s.resolver.DumpZones()
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
