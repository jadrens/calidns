package handler

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"

	"calidns/src/config"
	"calidns/src/geo"
	"calidns/src/recorder"
	"calidns/src/resolver"
)

// Handler implements dns.Handler for an authoritative DNS server.
type Handler struct {
	resolver *resolver.Resolver
	geo      *geo.Lookuper
	recorder *recorder.Recorder
	cfg      *config.Config
}

// New creates a new DNS handler.
func New(cfg *config.Config, resolverIns *resolver.Resolver, geoLookup *geo.Lookuper, rec *recorder.Recorder) *Handler {
	return &Handler{
		resolver: resolverIns,
		geo:      geoLookup,
		recorder: rec,
		cfg:      cfg,
	}
}

// ServeDNS handles an incoming DNS query.
func (h *Handler) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true
	m.RecursionAvailable = false

	// Default rcode — overridden on successful resolution.
	m.SetRcode(r, h.defaultRcode())

	if len(r.Question) == 0 {
		_ = w.WriteMsg(m)
		return
	}

	q := r.Question[0]
	domain := strings.ToLower(strings.TrimSuffix(q.Name, "."))
	qtype := q.Qtype
	qtypeStr := dns.TypeToString[qtype]

	// Extract client IP and EDNS info early.
	clientIP := extractIP(w.RemoteAddr())
	ednsSubnet := extractEDNSSubnet(r)
	nsid := extractNSID(r)

	// Resolve first with empty countryCode — fast_open zones ignore it anyway.
	// This lets us know whether geo lookup is needed before spending time on it.
	answer := h.resolver.Resolve(domain, qtype, "")

	// Determine whether geo lookup is necessary:
	// - Zone matched but not fast_open → geo needed for correct records.
	// - Zone matched and recording on → geo needed for log.
	// - No zone matched but default recording on → geo needed for log.
	needGeo := false
	needReresolve := false
	if answer != nil {
		needGeo = !answer.FastOpen || answer.Record
		needReresolve = !answer.FastOpen
	} else if h.cfg.Server.DefaultRecord && h.recorder != nil {
		needGeo = true
	}

	var countryCode, city, asn, asName string
	var cached, geoFailed bool
	var ednsCountry, ednsCity, ednsASN, ednsASName string
	var ednsGeoFailed bool

	if needGeo {
		var geoErr error
		countryCode, city, asn, asName, cached, geoErr = h.geo.Lookup(clientIP)
		geoFailed = geoErr != nil

		if ednsSubnet != "" {
			ecsIP := subnetFirstIP(ednsSubnet)
			if ecsIP != "" {
				var ecsErr error
				ednsCountry, ednsCity, ednsASN, ednsASName, _, ecsErr = h.geo.Lookup(ecsIP)
				ednsGeoFailed = ecsErr != nil
				if !ednsGeoFailed {
					countryCode = ednsCountry
					city = ednsCity
					asn = ednsASN
					asName = ednsASName
				}
			}
		}

		// Re-resolve with real country code if zone matched and not fast_open.
		if needReresolve {
			answer = h.resolver.Resolve(domain, qtype, countryCode)
		}
	} else {
		// Fast_open without recording — geo completely skipped, mark as failed for retry.
		geoFailed = true
	}

	if answer == nil {
		// No zone matched — record if default recording is enabled, then use default response.
		if h.cfg.Server.DefaultRecord && h.recorder != nil {
			h.recorder.Enqueue(&recorder.Entry{
				Domain:          domain,
				QueryType:       qtypeStr,
				ClientIP:        clientIP,
				CountryCode:     countryCode,
				City:            city,
				GeoCached:       cached,
				ASN:             asn,
				ASName:          asName,
				GeoFailed:       geoFailed || ednsGeoFailed,
				Timestamp:       time.Now(),
				EDNSSubnet:      ednsSubnet,
				EDNSCountryCode: ednsCountry,
				EDNSCity:        ednsCity,
				EDNSASN:         ednsASN,
				EDNSASName:      ednsASName,
				NSID:            nsid,
			})
		}
		_ = w.WriteMsg(m)
		return
	}

	// Record if enabled.
	if answer.Record && h.recorder != nil {
		recCountry := countryCode
		recCity := city
		recASN := asn
		recASName := asName
		recGeoFailed := geoFailed
		recEDNSCountry := ednsCountry
		recEDNSCity := ednsCity
		recEDNSASN := ednsASN
		recEDNSASName := ednsASName
		h.recorder.Enqueue(&recorder.Entry{
			Domain:          domain,
			QueryType:       qtypeStr,
			ClientIP:        clientIP,
			CountryCode:     recCountry,
			City:            recCity,
			GeoCached:       cached,
			ASN:             recASN,
			ASName:          recASName,
			GeoFailed:       recGeoFailed || ednsGeoFailed,
			Timestamp:       time.Now(),
			EDNSSubnet:      ednsSubnet,
			EDNSCountryCode: recEDNSCountry,
			EDNSCity:        recEDNSCity,
			EDNSASN:         recEDNSASN,
			EDNSASName:      recEDNSASName,
			NSID:            nsid,
		})
	}

	if len(answer.Records) == 0 {
		// Zone matched but no records for this qtype — NODATA.
		m.SetRcode(r, dns.RcodeSuccess)
		m.Ns = []dns.RR{soaRecord(domain, answer.TTL)}
	} else {
		m.SetRcode(r, dns.RcodeSuccess)
		m.Answer = answer.Records
	}

	_ = w.WriteMsg(m)
}

// defaultRcode returns the configured default response rcode.
func (h *Handler) defaultRcode() int {
	switch strings.ToLower(h.cfg.Server.DefaultResponse) {
	case "nxdomain":
		return dns.RcodeNameError
	case "servfail":
		return dns.RcodeServerFailure
	default:
		return dns.RcodeRefused
	}
}

// extractIP pulls the IP address from a remote address string (ip:port).
func extractIP(addr net.Addr) string {
	switch a := addr.(type) {
	case *net.UDPAddr:
		return a.IP.String()
	case *net.TCPAddr:
		return a.IP.String()
	default:
		if host, _, err := net.SplitHostPort(addr.String()); err == nil {
			return host
		}
		return addr.String()
	}
}

// extractEDNSSubnet returns the EDNS Client Subnet CIDR string from a DNS
// message, or empty string if not present.
func extractEDNSSubnet(r *dns.Msg) string {
	if opt := r.IsEdns0(); opt != nil {
		for _, o := range opt.Option {
			if subnet, ok := o.(*dns.EDNS0_SUBNET); ok {
				return fmt.Sprintf("%s/%d", subnet.Address.String(), subnet.SourceNetmask)
			}
		}
	}
	return ""
}

// extractNSID returns the Name Server Identifier from a DNS message's OPT
// record, or empty string if not present.
func extractNSID(r *dns.Msg) string {
	if opt := r.IsEdns0(); opt != nil {
		for _, o := range opt.Option {
			if nsid, ok := o.(*dns.EDNS0_NSID); ok {
				return nsid.Nsid
			}
		}
	}
	return ""
}

// subnetFirstIP returns the first usable IP address of a CIDR subnet.
// For "1.2.3.0/24" it returns "1.2.3.1". Returns empty string on parse error.
func subnetFirstIP(cidr string) string {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}
	ip4 := ipNet.IP.To4()
	if ip4 == nil {
		return ""
	}
	ip4[3] = 1
	return ip4.String()
}

// soaRecord builds a minimal SOA record for NODATA responses.
func soaRecord(domain string, ttl uint32) dns.RR {
	return &dns.SOA{
		Hdr: dns.RR_Header{
			Name:   dns.Fqdn(domain),
			Rrtype: dns.TypeSOA,
			Class:  dns.ClassINET,
			Ttl:    ttl,
		},
		Ns:      dns.Fqdn("ns1." + domain),
		Mbox:    dns.Fqdn("admin." + domain),
		Serial:  1,
		Refresh: 3600,
		Retry:   900,
		Expire:  86400,
		Minttl:  ttl,
	}
}
