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

	"dns-server/src/config"
	"dns-server/src/dnsdata"
	"dns-server/src/geo"
	"dns-server/src/recorder"
	"dns-server/src/resolver"
)

// Server handles HTTP API requests.
type Server struct {
	resolver  *resolver.Resolver
	recorder  *recorder.Recorder
	geoLookup *geo.Lookuper
	mux       *http.ServeMux
	tokens    map[string]struct{} // set of valid bearer tokens
	cfg       *config.Config      // server config (for persisting zone changes)
	dnsStore  *dnsdata.Store      // Web-API-managed DNS data

	// Cluster sync.
	clusterMode string   // "master", "slave", or empty
	slaves      []string // slave API domains (master mode)
	masterURL   string   // master API URL (slave mode)
	firstToken  string   // first token for forwarding

	// Precomputed CORS header values.
	corsOrigin      string
	corsMethods     string
	corsHeaders     string
	corsExpose      string
	corsMaxAge      int
	corsCredentials bool
}

// NewServer creates a new API server.
func NewServer(res *resolver.Resolver, rec *recorder.Recorder, geoLookup *geo.Lookuper, tokens []string, corsCfg config.CORSConfig, cfg *config.Config, dnsStore *dnsdata.Store) *Server {
	tokenSet := make(map[string]struct{}, len(tokens))
	for _, t := range tokens {
		if t = strings.TrimSpace(t); t != "" {
			tokenSet[t] = struct{}{}
		}
	}

	origin, methods, headers, expose, maxAge, creds := corsCfg.HeaderValues()

	s := &Server{
		resolver:        res,
		recorder:        rec,
		geoLookup:       geoLookup,
		mux:             http.NewServeMux(),
		tokens:          tokenSet,
		cfg:             cfg,
		dnsStore:        dnsStore,
		clusterMode:     cfg.Cluster.Mode,
		slaves:          cfg.Cluster.Slaves,
		masterURL:       cfg.Cluster.Master,
		firstToken:      cfg.Server.API.FirstToken(),
		corsOrigin:      origin,
		corsMethods:     methods,
		corsHeaders:     headers,
		corsExpose:      expose,
		corsMaxAge:      maxAge,
		corsCredentials: creds,
	}
	s.registerRoutes()
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.setCORSHeaders(w)

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Health check is public.
	if r.URL.Path == "/api/health" && r.Method == http.MethodGet {
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

	ze := resolver.ZoneEntry{
		Pattern:   req.Pattern,
		Regex:     req.Pattern,
		Mode:      req.Mode,
		Countries: req.Countries,
		TTL:       req.TTL,
		Record:    req.Record,
		FastOpen:  req.FastOpen,
	}

	if !s.resolver.UpsertZone(req.Pattern, ze) {
		writeError(w, http.StatusBadRequest, "invalid zone mode or pattern")
		return
	}

	log.Printf("[API] zone upserted: %s", req.Pattern)
	s.persistDNSData()
	s.forwardToSlaves(r.Method, r.URL.RequestURI(), bodyBytes)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "pattern": req.Pattern})
}

func (s *Server) deleteZone(w http.ResponseWriter, r *http.Request, pattern string) {
	if s.resolver.RemoveZone(pattern) {
		log.Printf("[API] zone deleted: %s", pattern)
		s.persistDNSData()
		s.forwardToSlaves(r.Method, r.URL.RequestURI(), nil)
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "pattern": pattern})
	} else {
		writeError(w, http.StatusNotFound, "zone not found")
	}
}

func (s *Server) deleteCountry(w http.ResponseWriter, r *http.Request, pattern, country string) {
	if s.resolver.RemoveCountry(pattern, country) {
		log.Printf("[API] country %q removed from zone %s", country, pattern)
		s.persistDNSData()
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
		stats := s.recorder.GetStats()
		resp["recorder"] = stats
	} else {
		resp["recorder"] = map[string]interface{}{"enabled": false}
	}

	writeJSON(w, http.StatusOK, resp)
}

// --- Health ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
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
		"listen":                s.cfg.Server.Listen,
		"default_ttl":           s.cfg.Server.DefaultTTL,
		"default_response":      s.cfg.Server.DefaultResponse,
		"default_record":        s.cfg.Server.DefaultRecord,
		"enable_geoip_mmap":     s.cfg.Server.EnableGeoIPMmap,
		"geoip_update_url":      s.cfg.Server.GeoIPUpdateURL,
		"geoip_update_interval": s.cfg.Server.GeoIPUpdateInterval,
	})
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

	if req.DefaultTTL != nil {
		if *req.DefaultTTL <= 0 {
			writeError(w, http.StatusBadRequest, "default_ttl must be positive")
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

	s.persistDNSData()
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
	writeJSON(w, http.StatusOK, s.cfg)
}

// --- Cluster forwarding ---

// forwardToSlaves sends the same request to all slave servers (best-effort).
func (s *Server) forwardToSlaves(method, path string, body []byte) {
	if s.clusterMode != "master" || len(s.slaves) == 0 {
		return
	}

	log.Printf("[cluster] forwarding %s %s to %d slave(s): %v", method, path, len(s.slaves), s.slaves)

	for _, slave := range s.slaves {
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
			continue
		}
		req.Header.Set("Authorization", "Bearer "+s.firstToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Cluster-Forward", "master")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Printf("[cluster] failed to forward to %s: %v", slave, err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			log.Printf("[cluster] slave %s returned %d for %s %s", slave, resp.StatusCode, method, path)
		} else {
			log.Printf("[cluster] forwarded to %s OK (%s %s)", slave, method, path)
		}
	}
}

// --- CORS ---

func (s *Server) setCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", s.corsOrigin)
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

func (s *Server) persistDNSData() {
	s.cfg.Zones = s.resolver.DumpZones()
	if s.dnsStore == nil {
		return
	}
	if err := s.dnsStore.SaveDefaults(dnsdata.Defaults{
		TTL: s.cfg.Server.DefaultTTL, Record: s.cfg.Server.DefaultRecord,
		Response: s.cfg.Server.DefaultResponse,
	}); err != nil {
		log.Printf("[API] failed to save DNS defaults: %v", err)
	}
	if err := s.dnsStore.ReplaceZones(s.cfg.Zones); err != nil {
		log.Printf("[API] failed to save DNS zones: %v", err)
	}
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
