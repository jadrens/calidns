package resolver

import (
	"fmt"
	"log"
	"net"
	"regexp"
	"strings"
	"sync"

	"github.com/miekg/dns"

	"calidns/src/config"
)

// Answer holds the resolved DNS answer.
type Answer struct {
	Records  []dns.RR
	TTL      uint32
	Record   bool
	FastOpen bool // zone-level fast_open was active for this resolution
}

// ZoneEntry is a public view of a zone for the API layer.
type ZoneEntry struct {
	Pattern   string               `json:"pattern"`
	Regex     string               `json:"regex"`
	Mode      string               `json:"mode,omitempty"`
	Countries map[string]RecordSet `json:"countries"`
	TTL       *int                 `json:"ttl,omitempty"`
	Record    *bool                `json:"record,omitempty"`
	FastOpen  *bool                `json:"fast_open,omitempty"`
}

// RecordSet mirrors config.RecordSet but with json tags.
type RecordSet struct {
	A     []string `json:"a,omitempty"`
	AAAA  []string `json:"aaaa,omitempty"`
	TXT   []string `json:"txt,omitempty"`
	CNAME []string `json:"cname,omitempty"`
	MX    []string `json:"mx,omitempty"`
	NS    []string `json:"ns,omitempty"`
	SRV   []string `json:"srv,omitempty"`
	CAA   []string `json:"caa,omitempty"`
	PTR   []string `json:"ptr,omitempty"`
	SOA   []string `json:"soa,omitempty"`
	Other []string `json:"other,omitempty"`
}

// Resolver matches domain names against configured zones and returns
// the appropriate DNS records for a given country code.
type Resolver struct {
	mu       sync.RWMutex
	zones    []zoneEntry // ordered list to preserve config file order
	defaults ResolutionDefaults
}

type zoneEntry struct {
	patternStr string
	pattern    *regexp.Regexp
	config     *config.ZoneConfig
}

// ResolutionDefaults holds the server-level fallback behaviour.
type ResolutionDefaults struct {
	TTL    uint32
	Record bool
}

// normalizePattern converts a user-friendly domain pattern into a regex.
// If the pattern already looks like a regex (contains \, ^, $, [, etc.),
// it is returned unchanged. Otherwise dots are escaped and anchors added
// so that "pp.self.rayou.me" matches only that exact domain.
func normalizePattern(p string) string {
	// Already looks like a hand-written regex — use as-is.
	if strings.ContainsAny(p, `\^$[]*+{}()|`) {
		return p
	}
	// Escape dots and add anchors with optional trailing dot.
	return "^" + strings.ReplaceAll(p, ".", "\\.") + "\\.?$"
}

// compilePattern applies the explicitly selected matching mode. An omitted
// mode retains the legacy auto-detection behaviour for existing configs.
func compilePattern(pattern, mode string) (*regexp.Regexp, string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case "simple":
		name := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(pattern)), ".")
		if name == "" {
			return nil, mode, fmt.Errorf("simple domain is empty")
		}
		re, err := regexp.Compile("^" + regexp.QuoteMeta(name) + "\\.?$")
		return re, mode, err
	case "golang":
		re, err := regexp.Compile(pattern)
		return re, mode, err
	case "":
		re, err := regexp.Compile(normalizePattern(pattern))
		return re, mode, err
	default:
		return nil, mode, fmt.Errorf("unknown zone mode %q", mode)
	}
}

// New creates a new Resolver from the server configuration.
func New(cfg *config.Config) *Resolver {
	r := &Resolver{
		defaults: ResolutionDefaults{
			TTL:    uint32(cfg.Server.DefaultTTL),
			Record: cfg.Server.DefaultRecord,
		},
	}

	for pattern, zc := range cfg.Zones {
		re, mode, err := compilePattern(pattern, zc.Mode)
		if err != nil {
			log.Printf("[DEBUG] failed to compile pattern %q with mode %q: %v", pattern, zc.Mode, err)
			continue
		}
		zc.Mode = mode
		r.zones = append(r.zones, zoneEntry{
			patternStr: pattern,
			pattern:    re,
			config:     zc,
		})
	}

	return r
}

// UpdateDefaults hot-reloads the server-level default TTL and record behaviour.
func (r *Resolver) UpdateDefaults(ttl uint32, record bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defaults.TTL = ttl
	r.defaults.Record = record
}

// Resolve finds matching records for a domain, query type, and country code.
func (r *Resolver) Resolve(domain string, qtype uint16, countryCode string) *Answer {
	r.mu.RLock()
	defer r.mu.RUnlock()

	domain = strings.ToLower(domain)

	for _, ze := range r.zones {
		if !ze.pattern.MatchString(domain) {
			continue
		}

		// Zone-level fast_open: skip geo, always use "default".
		fastOpen := false
		if ze.config.FastOpen != nil && *ze.config.FastOpen {
			fastOpen = true
			countryCode = ""
		}

		rs := r.selectRecordSet(ze.config, countryCode)

		ttl := r.defaults.TTL
		if ze.config.TTL != nil {
			ttl = uint32(*ze.config.TTL)
		}

		record := r.defaults.Record
		if ze.config.Record != nil {
			record = *ze.config.Record
		}

		records := buildRRs(domain, qtype, ttl, rs)

		return &Answer{
			Records:  records,
			TTL:      ttl,
			Record:   record,
			FastOpen: fastOpen,
		}
	}

	return nil
}

// ListZones returns all configured zones for API inspection.
func (r *Resolver) ListZones() []ZoneEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]ZoneEntry, 0, len(r.zones))
	for _, ze := range r.zones {
		countries := make(map[string]RecordSet, len(ze.config.Countries))
		for cc, rs := range ze.config.Countries {
			countries[cc] = RecordSet{
				A:     copySlice(rs.A),
				AAAA:  copySlice(rs.AAAA),
				TXT:   copySlice(rs.TXT),
				CNAME: copySlice(rs.CNAME),
				MX:    copySlice(rs.MX),
				NS:    copySlice(rs.NS),
				SRV:   copySlice(rs.SRV),
				CAA:   copySlice(rs.CAA),
				PTR:   copySlice(rs.PTR),
				SOA:   copySlice(rs.SOA),
				Other: copySlice(rs.Other),
			}
		}
		out = append(out, ZoneEntry{
			Pattern:   ze.patternStr,
			Regex:     ze.patternStr,
			Mode:      ze.config.Mode,
			Countries: countries,
			TTL:       ze.config.TTL,
			Record:    ze.config.Record,
			FastOpen:  ze.config.FastOpen,
		})
	}
	return out
}

// GetZone returns a single zone by its regex pattern string.
func (r *Resolver) GetZone(pattern string) *ZoneEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, ze := range r.zones {
		if ze.patternStr == pattern {
			countries := make(map[string]RecordSet, len(ze.config.Countries))
			for cc, rs := range ze.config.Countries {
				countries[cc] = RecordSet{
					A:     copySlice(rs.A),
					AAAA:  copySlice(rs.AAAA),
					TXT:   copySlice(rs.TXT),
					CNAME: copySlice(rs.CNAME),
					MX:    copySlice(rs.MX),
					NS:    copySlice(rs.NS),
					SRV:   copySlice(rs.SRV),
					CAA:   copySlice(rs.CAA),
					PTR:   copySlice(rs.PTR),
					SOA:   copySlice(rs.SOA),
					Other: copySlice(rs.Other),
				}
			}
			return &ZoneEntry{
				Pattern:   ze.patternStr,
				Regex:     ze.patternStr,
				Mode:      ze.config.Mode,
				Countries: countries,
				TTL:       ze.config.TTL,
				Record:    ze.config.Record,
				FastOpen:  ze.config.FastOpen,
			}
		}
	}
	return nil
}

// UpsertZone adds or replaces a zone. If the pattern already exists it is
// updated in-place; otherwise appended. Returns false if the regex is invalid.
func (r *Resolver) UpsertZone(pattern string, ze ZoneEntry) bool {
	re, mode, err := compilePattern(pattern, ze.Mode)
	if err != nil {
		return false
	}

	countries := make(map[string]*config.RecordSet, len(ze.Countries))
	for cc, rs := range ze.Countries {
		countries[cc] = &config.RecordSet{
			A:     copySlice(rs.A),
			AAAA:  copySlice(rs.AAAA),
			TXT:   copySlice(rs.TXT),
			CNAME: copySlice(rs.CNAME),
			MX:    copySlice(rs.MX),
			NS:    copySlice(rs.NS),
			SRV:   copySlice(rs.SRV),
			CAA:   copySlice(rs.CAA),
			PTR:   copySlice(rs.PTR),
			SOA:   copySlice(rs.SOA),
			Other: copySlice(rs.Other),
		}
	}

	zc := &config.ZoneConfig{
		Countries: countries,
		Mode:      mode,
		TTL:       ze.TTL,
		Record:    ze.Record,
		FastOpen:  ze.FastOpen,
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Debug: log stored record counts.
	for cc, rs := range countries {
		log.Printf("[DEBUG] upsertZone pattern=%s country=%s a=%d aaaa=%d txt=%d cname=%d mx=%d ns=%d srv=%d caa=%d ptr=%d soa=%d other=%d",
			pattern, cc, len(rs.A), len(rs.AAAA), len(rs.TXT), len(rs.CNAME),
			len(rs.MX), len(rs.NS), len(rs.SRV), len(rs.CAA), len(rs.PTR), len(rs.SOA), len(rs.Other))
	}

	// Replace existing.
	for i, z := range r.zones {
		if z.patternStr == pattern {
			r.zones[i] = zoneEntry{patternStr: pattern, pattern: re, config: zc}
			return true
		}
	}

	// Append new.
	r.zones = append(r.zones, zoneEntry{patternStr: pattern, pattern: re, config: zc})
	return true
}

// RemoveZone deletes a zone by its regex pattern string.
func (r *Resolver) RemoveZone(pattern string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i, z := range r.zones {
		if z.patternStr == pattern {
			r.zones = append(r.zones[:i], r.zones[i+1:]...)
			return true
		}
	}
	return false
}

// DumpZones returns all current zones as a map for config persistence.
func (r *Resolver) DumpZones() map[string]*config.ZoneConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make(map[string]*config.ZoneConfig, len(r.zones))
	for _, ze := range r.zones {
		// Deep copy so the caller owns the data.
		countries := make(map[string]*config.RecordSet, len(ze.config.Countries))
		for cc, rs := range ze.config.Countries {
			countries[cc] = &config.RecordSet{
				A:     copySlice(rs.A),
				AAAA:  copySlice(rs.AAAA),
				TXT:   copySlice(rs.TXT),
				CNAME: copySlice(rs.CNAME),
				MX:    copySlice(rs.MX),
				NS:    copySlice(rs.NS),
				SRV:   copySlice(rs.SRV),
				CAA:   copySlice(rs.CAA),
				PTR:   copySlice(rs.PTR),
				SOA:   copySlice(rs.SOA),
				Other: copySlice(rs.Other),
			}
		}
		zc := &config.ZoneConfig{
			Countries: countries,
			Mode:      ze.config.Mode,
		}
		if ze.config.TTL != nil {
			ttl := *ze.config.TTL
			zc.TTL = &ttl
		}
		if ze.config.Record != nil {
			rec := *ze.config.Record
			zc.Record = &rec
		}
		if ze.config.FastOpen != nil {
			fo := *ze.config.FastOpen
			zc.FastOpen = &fo
		}
		out[ze.patternStr] = zc
	}
	return out
}

// RemoveCountry deletes a country section from a zone. The "default" section
// cannot be removed.
func (r *Resolver) RemoveCountry(pattern, country string) bool {
	if strings.ToLower(country) == "default" {
		return false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, z := range r.zones {
		if z.patternStr == pattern {
			if _, ok := z.config.Countries[country]; ok {
				delete(z.config.Countries, country)
				return true
			}
			return false
		}
	}
	return false
}

// selectRecordSet picks the country-specific record set, falling back to "default".
func (r *Resolver) selectRecordSet(zc *config.ZoneConfig, countryCode string) *config.RecordSet {
	if countryCode != "" {
		if rs, ok := zc.Countries[countryCode]; ok {
			return rs
		}
	}
	if rs, ok := zc.Countries["default"]; ok {
		return rs
	}
	return nil
}

func buildRRs(domain string, qtype uint16, ttl uint32, rs *config.RecordSet) []dns.RR {
	if rs == nil {
		return nil
	}

	var rrs []dns.RR

	switch qtype {
	case dns.TypeA:
		for _, ip := range rs.A {
			ip = strings.TrimSpace(ip)
			rr := &dns.A{
				Hdr: dns.RR_Header{
					Name:   dns.Fqdn(domain),
					Rrtype: dns.TypeA,
					Class:  dns.ClassINET,
					Ttl:    ttl,
				},
			}
			if rr.A = net.ParseIP(ip); rr.A != nil {
				rrs = append(rrs, rr)
			} else {
				log.Printf("[DEBUG] buildRRs: skipping invalid A record %q for %s", ip, domain)
			}
		}
	case dns.TypeAAAA:
		for _, ip := range rs.AAAA {
			ip = strings.TrimSpace(ip)
			rr := &dns.AAAA{
				Hdr: dns.RR_Header{
					Name:   dns.Fqdn(domain),
					Rrtype: dns.TypeAAAA,
					Class:  dns.ClassINET,
					Ttl:    ttl,
				},
			}
			if rr.AAAA = net.ParseIP(ip); rr.AAAA != nil {
				rrs = append(rrs, rr)
			} else {
				log.Printf("[DEBUG] buildRRs: skipping invalid AAAA record %q for %s", ip, domain)
			}
		}
	case dns.TypeTXT:
		for _, txt := range rs.TXT {
			rr := &dns.TXT{
				Hdr: dns.RR_Header{
					Name:   dns.Fqdn(domain),
					Rrtype: dns.TypeTXT,
					Class:  dns.ClassINET,
					Ttl:    ttl,
				},
				Txt: []string{txt},
			}
			rrs = append(rrs, rr)
		}
	case dns.TypeCNAME:
		for _, cname := range rs.CNAME {
			rr := &dns.CNAME{
				Hdr: dns.RR_Header{
					Name:   dns.Fqdn(domain),
					Rrtype: dns.TypeCNAME,
					Class:  dns.ClassINET,
					Ttl:    ttl,
				},
				Target: dns.Fqdn(cname),
			}
			rrs = append(rrs, rr)
		}
	case dns.TypeMX:
		rrs = appendParsedRRs(rrs, domain, ttl, "MX", rs.MX)
	case dns.TypeNS:
		rrs = appendParsedRRs(rrs, domain, ttl, "NS", rs.NS)
	case dns.TypeSRV:
		rrs = appendParsedRRs(rrs, domain, ttl, "SRV", rs.SRV)
	case dns.TypeCAA:
		rrs = appendParsedRRs(rrs, domain, ttl, "CAA", rs.CAA)
	case dns.TypePTR:
		rrs = appendParsedRRs(rrs, domain, ttl, "PTR", rs.PTR)
	case dns.TypeSOA:
		rrs = appendParsedRRs(rrs, domain, ttl, "SOA", rs.SOA)
	}
	// Each "other" entry starts with its RR type, followed by the standard
	// zone-file RDATA. Only records matching the requested type are returned.
	for _, value := range rs.Other {
		rr, err := parseConfiguredRR(domain, ttl, value)
		if err != nil {
			log.Printf("[DEBUG] buildRRs: skipping invalid other record %q for %s: %v", value, domain, err)
			continue
		}
		if rr.Header().Rrtype == qtype {
			rrs = append(rrs, rr)
		}
	}

	return rrs
}

func appendParsedRRs(rrs []dns.RR, domain string, ttl uint32, rrType string, values []string) []dns.RR {
	for _, value := range values {
		rr, err := parseConfiguredRR(domain, ttl, rrType+" "+value)
		if err != nil {
			log.Printf("[DEBUG] buildRRs: skipping invalid %s record %q for %s: %v", rrType, value, domain, err)
			continue
		}
		if rr.Header().Rrtype != dns.StringToType[rrType] {
			log.Printf("[DEBUG] buildRRs: skipping mismatched %s record %q for %s", rrType, value, domain)
			continue
		}
		rrs = append(rrs, rr)
	}
	return rrs
}

// ValidateRecordSet rejects malformed structured and generic records before
// the API stores them. Existing A/AAAA/TXT/CNAME validation is unchanged.
func ValidateRecordSet(rs RecordSet) error {
	for _, group := range []struct {
		rrType string
		values []string
	}{
		{"MX", rs.MX}, {"NS", rs.NS}, {"SRV", rs.SRV},
		{"CAA", rs.CAA}, {"PTR", rs.PTR}, {"SOA", rs.SOA},
	} {
		for _, value := range group.values {
			rr, err := parseConfiguredRR("example.invalid", 300, group.rrType+" "+value)
			if err != nil {
				return fmt.Errorf("invalid %s record %q: %w", group.rrType, value, err)
			}
			if rr.Header().Rrtype != dns.StringToType[group.rrType] {
				return fmt.Errorf("invalid %s record %q: wrong RR type", group.rrType, value)
			}
		}
	}
	for _, value := range rs.Other {
		if _, err := parseConfiguredRR("example.invalid", 300, value); err != nil {
			return fmt.Errorf("invalid other record %q: %w", value, err)
		}
	}
	return nil
}

func parseConfiguredRR(domain string, ttl uint32, value string) (dns.RR, error) {
	if strings.ContainsAny(value, "\r\n") {
		return nil, fmt.Errorf("record must be a single line")
	}
	rr, err := dns.NewRR(fmt.Sprintf("%s %d IN %s", dns.Fqdn(domain), ttl, strings.TrimSpace(value)))
	if err != nil {
		return nil, err
	}
	if rr == nil || rr.Header().Class != dns.ClassINET || !strings.EqualFold(rr.Header().Name, dns.Fqdn(domain)) || rr.Header().Ttl != ttl {
		return nil, fmt.Errorf("record owner, class, or TTL does not match zone")
	}
	return rr, nil
}

func copySlice(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}
