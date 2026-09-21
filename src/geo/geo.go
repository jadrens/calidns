package geo

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// cacheEntry holds a cached geo lookup result (internal).
type cacheEntry struct {
	CountryCode string
	City        string
	ASN         string
	ASName      string
	ExpiresAt   time.Time
}

// CacheEntry is a public view of a cached geo lookup, keyed by /24 subnet.
type CacheEntry struct {
	Subnet      string `json:"subnet"` // e.g. "1.2.3.0/24"
	CountryCode string `json:"country_code"`
	City        string `json:"city"`
	ASN         string `json:"asn"`
	ASName      string `json:"as_name"`
	ExpiresAt   string `json:"expires_at"` // RFC3339
}

type providerFlight struct {
	done   chan struct{}
	result ProviderResult
	err    error
}

// Lookuper checks the in-memory and SQLite /24 cache before the local
// geoip.dat index, then uses the API only when the local index misses.
type Lookuper struct {
	mu             sync.RWMutex
	cache          map[uint32]*cacheEntry // key: subnet24 (first 24 bits of IPv4)
	v6Cache        map[string]*cacheEntry
	flightMu       sync.Mutex
	flights        map[string]*providerFlight
	provider       Provider
	ttl            time.Duration
	db             *sql.DB
	stopCh         chan struct{}
	stopOnce       sync.Once
	workers        sync.WaitGroup
	disabled       atomic.Bool
	localMu        sync.RWMutex
	local          *datIndex
	datPath        string
	mmap           bool
	updateURL      string
	updateInterval time.Duration
	updateCancel   func()
}

// NewLookuper creates a new geo lookuper.
// dbPath is the path to the SQLite cache database file.
// provider is an optional vendor-neutral external fallback.
// datPath is the path to an optional geoip.dat file. When enableMmap is true,
// the compact lookup index is file-backed instead of retained on the Go heap.
func NewLookuper(dbPath string, ttl time.Duration, provider Provider, datPath string, enableMmap bool, updateURL string, updateInterval time.Duration) (*Lookuper, error) {
	if ttl <= 0 {
		return nil, fmt.Errorf("geo cache TTL must be positive")
	}
	if updateURL != "" && (datPath == "" || updateInterval <= 0) {
		return nil, fmt.Errorf("geoip.dat update requires a file path and positive interval")
	}
	var local *datIndex
	if datPath != "" {
		var err error
		local, err = loadDatIndex(datPath, enableMmap)
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("loading geoip.dat: %w", err)
		}
		if os.IsNotExist(err) {
			log.Printf("[geo] %s not found; using SQLite/API only", datPath)
		}
		if local != nil && enableMmap {
			// The temporary heap index used to build the mapped index is no
			// longer needed. Return its pages to the OS at startup.
			debug.FreeOSMemory()
		}
	}

	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_synchronous=NORMAL")
	if err != nil {
		if local != nil {
			_ = local.Close()
		}
		return nil, fmt.Errorf("geo cache sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)

	schema := `
	CREATE TABLE IF NOT EXISTS geo_cache (
		subnet24     INTEGER PRIMARY KEY,
		country_code TEXT NOT NULL DEFAULT '',
		city         TEXT NOT NULL DEFAULT '',
		asn          INTEGER DEFAULT NULL,
		as_name      TEXT NOT NULL DEFAULT '',
		expires_at   DATETIME NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_geo_cache_expires ON geo_cache(expires_at);
	`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		if local != nil {
			_ = local.Close()
		}
		return nil, fmt.Errorf("geo cache schema: %w", err)
	}

	// Remove any already-expired entries on startup.
	if _, err := db.Exec("DELETE FROM geo_cache WHERE expires_at < ?", time.Now().UTC().Format(time.RFC3339)); err != nil {
		db.Close()
		if local != nil {
			_ = local.Close()
		}
		return nil, fmt.Errorf("purging expired geo cache: %w", err)
	}

	l := &Lookuper{
		cache:          make(map[uint32]*cacheEntry),
		v6Cache:        make(map[string]*cacheEntry),
		flights:        make(map[string]*providerFlight),
		provider:       provider,
		ttl:            ttl,
		db:             db,
		stopCh:         make(chan struct{}),
		local:          local,
		datPath:        datPath,
		mmap:           enableMmap,
		updateURL:      updateURL,
		updateInterval: updateInterval,
	}

	// Background goroutine to periodically purge expired entries.
	l.workers.Add(1)
	go func() {
		defer l.workers.Done()
		l.purgeLoop()
	}()
	if updateURL != "" && updateInterval > 0 {
		ctx, cancel := context.WithCancel(context.Background())
		l.updateCancel = cancel
		l.workers.Add(1)
		go func() {
			defer l.workers.Done()
			l.updateLoop(ctx)
		}()
	}

	return l, nil
}

// Close shuts down the background goroutine and closes the database.
func (l *Lookuper) Close() error {
	l.stopOnce.Do(func() {
		close(l.stopCh)
		if l.updateCancel != nil {
			l.updateCancel()
		}
	})
	l.workers.Wait()
	dbErr := l.db.Close()
	l.localMu.Lock()
	defer l.localMu.Unlock()
	if l.local != nil {
		if err := l.local.Close(); err != nil {
			return err
		}
	}
	return dbErr
}

// Disable disables geo lookups (all IPs return empty).
func (l *Lookuper) Disable() {
	l.disabled.Store(true)
}

func (l *Lookuper) lookupLocal(ip string) string {
	l.localMu.RLock()
	defer l.localMu.RUnlock()
	if l.local == nil {
		return ""
	}
	return l.local.lookup(ip)
}

// subnet24 extracts the first 24 bits of an IPv4 address as an integer.
// Returns 0 and false for non-IPv4 addresses.
func subnet24(ip string) (uint32, bool) {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return 0, false
	}
	// Convert to 4-byte representation.
	ip4 := parsed.To4()
	if ip4 == nil {
		return 0, false
	}
	// Pack first 3 bytes into a uint32: [b0][b1][b2]
	return uint32(ip4[0])<<16 | uint32(ip4[1])<<8 | uint32(ip4[2]), true
}

// Lookup returns the country code, city, ASN, and AS for a given IP.
// API results are cached in-memory and in SQLite (keyed by /24 subnet).
// Local geoip.dat results return country only and are not persisted.
// cached reports whether the result came from the in-memory or SQLite cache.
func (l *Lookuper) Lookup(ip string) (countryCode, city, asn, asName string, cached bool, err error) {
	if l.disabled.Load() || ip == "" {
		return "", "", "", "", false, nil
	}

	// Extract /24 subnet for cache key.
	sn, ok := subnet24(ip)
	if !ok {
		parsed := net.ParseIP(ip)
		if parsed == nil {
			return "", "", "", "", false, nil
		}
		key := parsed.String()
		now := time.Now()
		l.mu.RLock()
		entry, found := l.v6Cache[key]
		l.mu.RUnlock()
		if found && now.Before(entry.ExpiresAt) {
			return entry.CountryCode, entry.City, entry.ASN, entry.ASName, true, nil
		}
		// IPv6 cache is in-memory because the legacy SQLite schema is keyed by
		// an IPv4 /24 integer.
		if cc := l.lookupLocal(ip); cc != "" {
			return cc, "", "", "", false, nil
		}
		if l.provider == nil {
			return "", "", "", "", false, nil
		}
		result, err := l.lookupProvider("v6:"+key, ip)
		cc, city, asnVal, asVal, _, lookupErr := providerValues(result, false, err)
		if lookupErr == nil {
			l.mu.Lock()
			l.v6Cache[key] = &cacheEntry{CountryCode: cc, City: city, ASN: asnVal, ASName: asVal, ExpiresAt: now.Add(l.ttl)}
			l.mu.Unlock()
		}
		return cc, city, asnVal, asVal, false, lookupErr
	}

	// 1. Check in-memory cache.
	nowTime := time.Now()
	l.mu.RLock()
	if entry, ok := l.cache[sn]; ok && nowTime.Before(entry.ExpiresAt) {
		l.mu.RUnlock()
		return entry.CountryCode, entry.City, entry.ASN, entry.ASName, true, nil
	}
	l.mu.RUnlock()

	// 2. Check SQLite cache.
	now := time.Now().UTC()
	var cachedCC, cachedCity, cachedASN, cachedASName string
	var expiresAt time.Time
	err = l.db.QueryRow(
		"SELECT country_code, city, COALESCE(CAST(asn AS TEXT), ''), COALESCE(as_name,''), expires_at FROM geo_cache WHERE subnet24 = ?",
		sn,
	).Scan(&cachedCC, &cachedCity, &cachedASN, &cachedASName, &expiresAt)

	if err == nil && now.Before(expiresAt) {
		// SQLite cache hit — populate in-memory cache and return.
		l.mu.Lock()
		l.cache[sn] = &cacheEntry{
			CountryCode: cachedCC,
			City:        cachedCity,
			ASN:         cachedASN,
			ASName:      cachedASName,
			ExpiresAt:   expiresAt,
		}
		l.mu.Unlock()
		return cachedCC, cachedCity, cachedASN, cachedASName, true, nil
	}

	// Expired entry in SQLite — delete it.
	if err == nil {
		if _, deleteErr := l.db.Exec("DELETE FROM geo_cache WHERE subnet24 = ?", sn); deleteErr != nil {
			log.Printf("[geo] deleting expired cache entry %s failed: %v", subnet24ToCIDR(sn), deleteErr)
		}
	}

	// 3. Use the local country database without adding its country-only result
	// to the SQLite cache (which can also hold richer API data).
	if cc := l.lookupLocal(ip); cc != "" {
		return cc, "", "", "", false, nil
	}
	if l.provider == nil {
		return "", "", "", "", false, nil
	}

	// 4. Use the injected external provider only if the local database misses.
	result, err := l.lookupProvider(fmt.Sprintf("v4:%d", sn), ip)
	if err != nil {
		return "", "", "", "", false, err
	}
	cc, city, asnVal, asVal, _, _ := providerValues(result, false, nil)

	expireTime := now.Add(l.ttl)

	// Store in SQLite (upsert by subnet24).
	if _, err := l.db.Exec(
		`INSERT OR REPLACE INTO geo_cache (subnet24, country_code, city, asn, as_name, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		sn, cc, city, asnVal, asVal, expireTime.Format(time.RFC3339),
	); err != nil {
		return "", "", "", "", false, fmt.Errorf("storing geo cache entry: %w", err)
	}

	// Store in memory.
	l.mu.Lock()
	l.cache[sn] = &cacheEntry{
		CountryCode: cc,
		City:        city,
		ASN:         asnVal,
		ASName:      asVal,
		ExpiresAt:   expireTime,
	}
	l.mu.Unlock()

	return cc, city, asnVal, asVal, false, nil
}

func (l *Lookuper) lookupProvider(key, ip string) (ProviderResult, error) {
	l.flightMu.Lock()
	if flight := l.flights[key]; flight != nil {
		l.flightMu.Unlock()
		<-flight.done
		return flight.result, flight.err
	}
	flight := &providerFlight{done: make(chan struct{})}
	l.flights[key] = flight
	l.flightMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	flight.result, flight.err = l.provider.Lookup(ctx, ip)
	cancel()
	l.flightMu.Lock()
	delete(l.flights, key)
	close(flight.done)
	l.flightMu.Unlock()
	return flight.result, flight.err
}

func providerValues(result ProviderResult, cached bool, err error) (country, region, asn, asnName string, wasCached bool, lookupErr error) {
	if result.CountryCode != nil {
		country = *result.CountryCode
	}
	if result.SubLocation != nil {
		region = *result.SubLocation
	}
	if result.ASN != nil {
		asn = *result.ASN
	}
	if result.ASNName != nil {
		asnName = *result.ASNName
	}
	return country, region, asn, asnName, cached, err
}

// subnet24ToCIDR converts a 24-bit integer back to "x.y.z.0/24" notation.
func subnet24ToCIDR(sn uint32) string {
	return fmt.Sprintf("%d.%d.%d.0/24", sn>>16, (sn>>8)&0xff, sn&0xff)
}

// ListCache returns cached entries from SQLite (excluding expired) with
// optional pagination. An optional countryCode filter limits results to that
// country. Returns the matching entries, the total count (ignoring pagination),
// and any error.
func (l *Lookuper) ListCache(countryCode string, limit, offset int) ([]CacheEntry, int, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	// Build WHERE clause.
	where := "expires_at >= ?"
	args := []interface{}{now}
	if countryCode != "" {
		where += " AND country_code = ?"
		args = append(args, countryCode)
	}

	// Total count (ignoring pagination).
	var total int
	countQuery := "SELECT COUNT(*) FROM geo_cache WHERE " + where
	if err := l.db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting geo cache: %w", err)
	}

	// Paginated query.
	dataQuery := "SELECT subnet24, country_code, city, COALESCE(CAST(asn AS TEXT),''), COALESCE(as_name,''), expires_at FROM geo_cache WHERE " +
		where + " ORDER BY subnet24"

	if limit > 0 {
		dataQuery += " LIMIT ?"
		args = append(args, limit)
	}
	if offset > 0 {
		dataQuery += " OFFSET ?"
		args = append(args, offset)
	}

	rows, err := l.db.Query(dataQuery, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("listing geo cache: %w", err)
	}
	defer rows.Close()

	var entries []CacheEntry
	for rows.Next() {
		var sn uint32
		var cc, city, asn, asName string
		var expiresAt time.Time
		if err := rows.Scan(&sn, &cc, &city, &asn, &asName, &expiresAt); err != nil {
			continue
		}
		entries = append(entries, CacheEntry{
			Subnet:      subnet24ToCIDR(sn),
			CountryCode: cc,
			City:        city,
			ASN:         asn,
			ASName:      asName,
			ExpiresAt:   expiresAt.Format(time.RFC3339),
		})
	}
	if entries == nil {
		entries = []CacheEntry{}
	}
	return entries, total, nil
}

// DeleteCache removes cached entries by subnet and/or country code.
// - No subnet, no countryCode → deletes all entries.
// - With subnet → deletes that specific /24 subnet.
// - With countryCode → deletes all entries matching that country.
// - With both → deletes the subnet within that country.
// The subnet should be in CIDR notation like "1.2.3.0/24".
func (l *Lookuper) DeleteCache(subnet, countryCode string) (int64, error) {
	// Build WHERE clause dynamically.
	where := "1=1"
	args := make([]interface{}, 0)

	var sn uint32
	if subnet != "" {
		// Parse the subnet string back to a 24-bit integer.
		var ok bool
		sn, ok = subnet24(subnet)
		if !ok {
			_, ipNet, err := net.ParseCIDR(subnet)
			if err != nil {
				return 0, fmt.Errorf("invalid subnet: %q (expected e.g. 1.2.3.0/24)", subnet)
			}
			ip4 := ipNet.IP.To4()
			if ip4 == nil {
				return 0, fmt.Errorf("only IPv4 subnets are supported")
			}
			sn = uint32(ip4[0])<<16 | uint32(ip4[1])<<8 | uint32(ip4[2])
		}
		where += " AND subnet24 = ?"
		args = append(args, sn)
	}
	if countryCode != "" {
		where += " AND country_code = ?"
		args = append(args, countryCode)
	}

	delQuery := fmt.Sprintf("DELETE FROM geo_cache WHERE %s", where)
	res, err := l.db.Exec(delQuery, args...)
	if err != nil {
		return 0, fmt.Errorf("deleting geo cache: %w", err)
	}
	n, _ := res.RowsAffected()

	// Also clear in-memory cache.
	l.mu.Lock()
	if subnet == "" && countryCode == "" {
		// Delete all.
		l.cache = make(map[uint32]*cacheEntry)
	} else if subnet != "" {
		delete(l.cache, sn)
	} else {
		// Delete by country — iterate through map.
		for k, entry := range l.cache {
			if entry.CountryCode == countryCode {
				delete(l.cache, k)
			}
		}
	}
	l.mu.Unlock()

	return n, nil
}

// purgeLoop periodically removes expired entries from SQLite.
func (l *Lookuper) purgeLoop() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-l.stopCh:
			return
		case <-ticker.C:
			now := time.Now()
			_, _ = l.db.Exec("DELETE FROM geo_cache WHERE expires_at < ?",
				now.UTC().Format(time.RFC3339))
			l.purgeExpiredMemoryEntries(now)
		}
	}
}

// purgeExpiredMemoryEntries removes entries that Lookup would no longer
// consider valid. It is kept separate from the SQLite cleanup so the in-memory
// cache cannot grow indefinitely as subnets expire.
func (l *Lookuper) purgeExpiredMemoryEntries(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for subnet, entry := range l.cache {
		if entry == nil || !now.Before(entry.ExpiresAt) {
			delete(l.cache, subnet)
		}
	}
}
