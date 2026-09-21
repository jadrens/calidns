package recorder

import (
	"database/sql"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/lib/pq"
)

// geoLookup interface avoids circular imports.
type GeoLookup interface {
	Lookup(ip string) (countryCode, city, asn, asName string, cached bool, err error)
}

// Entry represents a single DNS query record.
type Entry struct {
	ID              int64     `json:"id"`
	Domain          string    `json:"domain"`
	QueryType       string    `json:"query_type"`
	ClientIP        string    `json:"client_ip"`
	CountryCode     string    `json:"country_code"`
	City            string    `json:"city"`
	GeoCached       bool      `json:"geo_cached"` // true if geo lookup was served from cache
	ASN             string    `json:"asn,omitempty"`
	ASName          string    `json:"as_name,omitempty"`
	EDNSSubnet      string    `json:"edns_subnet,omitempty"`
	EDNSCountryCode string    `json:"edns_country_code,omitempty"`
	EDNSCity        string    `json:"edns_city,omitempty"`
	EDNSASN         string    `json:"edns_asn,omitempty"`
	EDNSASName      string    `json:"edns_as_name,omitempty"`
	NSID            string    `json:"nsid,omitempty"`
	GeoFailed       bool      `json:"-"` // true if geo lookup failed, needs retry
	CreatedAt       string    `json:"created_at"`
	Timestamp       time.Time `json:"-"`
}

// QueryResult holds paginated query history results.
type QueryResult struct {
	Total int      `json:"total"`
	Items []*Entry `json:"items"`
}

// EDNSRecord holds a single EDNS-enriched query record (INNER JOIN view).
type EDNSRecord struct {
	ID              int64  `json:"id"`
	Domain          string `json:"domain"`
	QueryType       string `json:"query_type"`
	ClientIP        string `json:"client_ip"`
	CountryCode     string `json:"country_code"`
	City            string `json:"city"`
	Subnet          string `json:"subnet"`
	EDNSCountryCode string `json:"edns_country_code"`
	EDNSCity        string `json:"edns_city,omitempty"`
	EDNSASN         string `json:"edns_asn,omitempty"`
	EDNSASName      string `json:"edns_as_name,omitempty"`
	NSID            string `json:"nsid"`
	CreatedAt       string `json:"created_at"`
}

// EDNSResult holds paginated EDNS query results.
type EDNSResult struct {
	Total int           `json:"total"`
	Items []*EDNSRecord `json:"items"`
}

// Stats holds recorder statistics.
type Stats struct {
	QueueLen     int   `json:"queue_len"`
	TotalQueries int64 `json:"total_queries"`
	CacheHited   int64 `json:"cache_hited"`
	Dropped      int64 `json:"dropped"`
}

// Recorder writes DNS query entries to PostgreSQL or SQLite asynchronously.
// A collector goroutine reads from ch and dispatches entries to a worker pool
// that performs per-entry inserts, decoupling I/O from data reception.
type Recorder struct {
	db      *sql.DB
	dbType  string
	ch      chan *Entry
	taskCh  chan *Entry
	wg      sync.WaitGroup
	stop    chan struct{}
	once    sync.Once
	workers int

	// geo retry
	geo     GeoLookup
	retryCh chan *geoRetryJob

	// atomic metrics
	dropped int64
}

// geoRetryJob holds the info needed to retry a failed geo lookup.
type geoRetryJob struct {
	QueryID      int64
	ClientIP     string
	EDNSSubnet   string
	EDNSSubnetIP string // first IP of EDNS subnet, or empty
}

// New opens the selected database, creates the schema, and starts the
// background writer goroutine. geo can be nil to disable retry.
func New(dbType, dsn string, geo GeoLookup) (*Recorder, error) {
	var db *sql.DB
	var err error
	if dbType == "sqlite" {
		db, err = openSQLite(dsn)
	} else {
		db, err = sql.Open("postgres", dsn)
		dbType = "postgres"
	}
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", dbType, err)
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging %s: %w", dbType, err)
	}

	schema := `
	CREATE TABLE IF NOT EXISTS queries (
		id BIGSERIAL PRIMARY KEY,
		domain TEXT NOT NULL,
		query_type TEXT NOT NULL,
		client_ip INET DEFAULT '0.0.0.0',
		country_code TEXT DEFAULT 'NO',
		city TEXT DEFAULT 'empty',
		geo_cached BOOLEAN NOT NULL DEFAULT FALSE,
		edns_included BOOLEAN NOT NULL DEFAULT FALSE,
		asn TEXT DEFAULT NULL,
		as_name TEXT DEFAULT 'empty',
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_domain ON queries(domain);
	CREATE INDEX IF NOT EXISTS idx_created_at ON queries(created_at);

	CREATE TABLE IF NOT EXISTS edns (
		id BIGINT PRIMARY KEY,
		subnet INET DEFAULT NULL,
		country_code TEXT NOT NULL DEFAULT 'NO',
		city TEXT NOT NULL DEFAULT 'empty',
		asn TEXT DEFAULT NULL,
		as_name TEXT DEFAULT 'empty',
		nsid TEXT NOT NULL DEFAULT 'empty',
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);
	`
	if dbType == "sqlite" {
		schema = sqliteSchema
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating schema: %w", err)
	}
	if dbType == "postgres" {
		// Older CaliDNS releases created ASN columns as INTEGER even though the
		// provider and public API model ASN values as strings. Migrate existing
		// installations before writers start so empty and vendor-formatted ASN
		// values cannot make every query insert fail.
		for _, table := range []string{"queries", "edns"} {
			var dataType string
			err := db.QueryRow(`SELECT data_type FROM information_schema.columns
				WHERE table_schema = current_schema() AND table_name = $1 AND column_name = 'asn'`, table).Scan(&dataType)
			if err != nil {
				db.Close()
				return nil, fmt.Errorf("checking %s ASN schema: %w", table, err)
			}
			if dataType != "text" {
				if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ALTER COLUMN asn TYPE TEXT USING asn::TEXT", table)); err != nil {
					db.Close()
					return nil, fmt.Errorf("migrating %s ASN schema: %w", table, err)
				}
			}
		}
	}

	const numWorkers = 4

	r := &Recorder{
		db:      db,
		dbType:  dbType,
		ch:      make(chan *Entry, 1024),
		taskCh:  make(chan *Entry, 1024),
		stop:    make(chan struct{}),
		workers: numWorkers,
		geo:     geo,
		retryCh: make(chan *geoRetryJob, 256),
	}

	// Start collector goroutine.
	r.wg.Add(1)
	go r.collect()

	// Start worker pool.
	for i := 0; i < numWorkers; i++ {
		r.wg.Add(1)
		go r.worker()
	}

	// Start geo retry worker.
	if geo != nil {
		r.wg.Add(1)
		go r.geoRetryWorker()
	}

	return r, nil
}

// Enqueue adds an entry to the write queue.
func (r *Recorder) Enqueue(e *Entry) {
	select {
	case r.ch <- e:
	default:
		dropped := atomic.AddInt64(&r.dropped, 1)
		if dropped == 1 || dropped%100 == 0 {
			log.Printf("[recorder] input queue full (dropped=%d)", dropped)
		}
	}
}

// Close flushes pending entries and shuts down the writer goroutine.
func (r *Recorder) Close() error {
	r.once.Do(func() {
		close(r.stop)
		r.wg.Wait()
	})
	if err := r.db.Close(); err != nil {
		return fmt.Errorf("closing db: %w", err)
	}
	return nil
}

// QueueLen returns the current queue length.
func (r *Recorder) QueueLen() int {
	return len(r.ch)
}

// GetStats returns recorder statistics and reports database failures.
func (r *Recorder) GetStats() (Stats, error) {
	var total, hits int64
	if err := r.db.QueryRow("SELECT COUNT(*) FROM queries").Scan(&total); err != nil {
		return Stats{}, fmt.Errorf("counting queries: %w", err)
	}
	if err := r.db.QueryRow("SELECT COUNT(*) FROM queries WHERE geo_cached = TRUE").Scan(&hits); err != nil {
		return Stats{}, fmt.Errorf("counting cache hits: %w", err)
	}

	return Stats{
		QueueLen:     len(r.ch),
		TotalQueries: total,
		CacheHited:   hits,
		Dropped:      atomic.LoadInt64(&r.dropped),
	}, nil
}

// Ping verifies that the recorder database is reachable.
func (r *Recorder) Ping() error {
	return r.db.Ping()
}

// QueryHistory returns paginated query history with optional filters.
// Empty filters are ignored; empty everything returns all records.
func (r *Recorder) QueryHistory(id int64, domain, clientIP, subnet, countryCode, start, end string, limit, offset int) (*QueryResult, error) {
	where := "1=1"
	args := make([]interface{}, 0)
	ph := 0 // positional-parameter counter

	if id > 0 {
		ph++
		where += fmt.Sprintf(" AND q.id = $%d", ph)
		args = append(args, id)
	}
	if domain != "" {
		ph++
		if r.dbType == "sqlite" {
			where += fmt.Sprintf(" AND LOWER(q.domain) LIKE LOWER($%d)", ph)
		} else {
			where += fmt.Sprintf(" AND q.domain ILIKE $%d", ph)
		}
		args = append(args, "%"+domain+"%")
	}
	if clientIP != "" {
		ph++
		if r.dbType == "sqlite" {
			where += fmt.Sprintf(" AND LOWER(q.client_ip) LIKE LOWER($%d)", ph)
		} else {
			where += fmt.Sprintf(" AND q.client_ip::TEXT ILIKE $%d", ph)
		}
		args = append(args, "%"+clientIP+"%")
	}
	if subnet != "" {
		ph++
		if r.dbType == "sqlite" {
			where += fmt.Sprintf(" AND ip_in_subnet(q.client_ip, $%d)", ph)
		} else {
			// <<= : q.client_ip is contained by or equals the given subnet
			where += fmt.Sprintf(" AND q.client_ip <<= $%d", ph)
		}
		args = append(args, subnet)
	}
	if countryCode != "" {
		ph++
		where += fmt.Sprintf(" AND q.country_code = $%d", ph)
		args = append(args, countryCode)
	}
	if start != "" {
		ph++
		where += fmt.Sprintf(" AND q.created_at >= $%d", ph)
		args = append(args, r.timeFilter(start))
	}
	if end != "" {
		ph++
		where += fmt.Sprintf(" AND q.created_at <= $%d", ph)
		args = append(args, r.timeFilter(end))
	}

	// Count total.
	var total int64
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM queries q WHERE %s", where)
	if err := r.db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("counting: %w", err)
	}

	// Fetch page.
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}

	ph++
	limitPh := ph
	ph++
	offsetPh := ph

	queryASN, ednsSubnet, ednsASN := "q.asn::TEXT", "e.subnet::TEXT", "e.asn::TEXT"
	if r.dbType == "sqlite" {
		queryASN, ednsSubnet, ednsASN = "CAST(q.asn AS TEXT)", "e.subnet", "CAST(e.asn AS TEXT)"
	}
	dataQuery := fmt.Sprintf(
		`SELECT q.id, q.domain, q.query_type, q.client_ip, q.country_code, q.city, q.geo_cached,
		 COALESCE(%s,''), COALESCE(q.as_name,''), q.created_at,
		 COALESCE(%s, ''), COALESCE(e.country_code, ''), COALESCE(e.city, ''), COALESCE(%s,''), COALESCE(e.as_name,''), COALESCE(e.nsid, '')
		 FROM queries q
		 LEFT JOIN edns e ON q.id = e.id
		 WHERE %s ORDER BY q.created_at DESC LIMIT $%d OFFSET $%d`,
		queryASN, ednsSubnet, ednsASN, where, limitPh, offsetPh,
	)
	dataArgs := append(args, limit, offset)

	rows, err := r.db.Query(dataQuery, dataArgs...)
	if err != nil {
		return nil, fmt.Errorf("querying: %w", err)
	}
	defer rows.Close()

	items := make([]*Entry, 0)
	for rows.Next() {
		var e Entry
		var asn, asName, ednsSubnet, ednsCC, ednsCity, ednsASN, ednsASName, nsid string
		if err := rows.Scan(&e.ID, &e.Domain, &e.QueryType, &e.ClientIP, &e.CountryCode, &e.City, &e.GeoCached,
			&asn, &asName, &e.CreatedAt,
			&ednsSubnet, &ednsCC, &ednsCity, &ednsASN, &ednsASName, &nsid); err != nil {
			return nil, fmt.Errorf("scanning query history: %w", err)
		}
		e.ASN = asn
		e.ASName = asName
		e.EDNSSubnet = ednsSubnet
		e.EDNSCountryCode = ednsCC
		e.EDNSCity = ednsCity
		e.EDNSASN = ednsASN
		e.EDNSASName = ednsASName
		e.NSID = nsid
		items = append(items, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating query history: %w", err)
	}

	return &QueryResult{Total: int(total), Items: items}, nil
}

// QueryEDNS returns paginated EDNS-enriched query records (only queries that
// carried EDNS options). Supports filtering by id, subnet, country_code, nsid,
// and a time window via start/end.
func (r *Recorder) QueryEDNS(id int64, subnet, countryCode, nsid, start, end string, limit, offset int) (*EDNSResult, error) {
	where := "1=1"
	args := make([]interface{}, 0)
	ph := 0

	if id > 0 {
		ph++
		where += fmt.Sprintf(" AND e.id = $%d", ph)
		args = append(args, id)
	}
	if subnet != "" {
		ph++
		where += fmt.Sprintf(" AND e.subnet = $%d", ph)
		args = append(args, subnet)
	}
	if countryCode != "" {
		ph++
		where += fmt.Sprintf(" AND e.country_code = $%d", ph)
		args = append(args, countryCode)
	}
	if nsid != "" {
		ph++
		where += fmt.Sprintf(" AND e.nsid = $%d", ph)
		args = append(args, nsid)
	}
	if start != "" {
		ph++
		where += fmt.Sprintf(" AND q.created_at >= $%d", ph)
		args = append(args, r.timeFilter(start))
	}
	if end != "" {
		ph++
		where += fmt.Sprintf(" AND q.created_at <= $%d", ph)
		args = append(args, r.timeFilter(end))
	}

	// Count total.
	var total int64
	countQuery := fmt.Sprintf(
		"SELECT COUNT(*) FROM edns e INNER JOIN queries q ON e.id = q.id WHERE %s", where,
	)
	if err := r.db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("counting edns: %w", err)
	}

	// Fetch page.
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}

	ph++
	limitPh := ph
	ph++
	offsetPh := ph

	ednsASN := "e.asn::TEXT"
	if r.dbType == "sqlite" {
		ednsASN = "CAST(e.asn AS TEXT)"
	}
	dataQuery := fmt.Sprintf(
		`SELECT e.id, q.domain, q.query_type, q.client_ip, q.country_code, q.city,
		 e.subnet, e.country_code, e.city, COALESCE(%s,''), COALESCE(e.as_name,''), e.nsid, q.created_at
		 FROM edns e
		 INNER JOIN queries q ON e.id = q.id
		 WHERE %s ORDER BY q.created_at DESC LIMIT $%d OFFSET $%d`,
		ednsASN, where, limitPh, offsetPh,
	)
	dataArgs := append(args, limit, offset)

	rows, err := r.db.Query(dataQuery, dataArgs...)
	if err != nil {
		return nil, fmt.Errorf("querying edns: %w", err)
	}
	defer rows.Close()

	items := make([]*EDNSRecord, 0)
	for rows.Next() {
		var rec EDNSRecord
		var ednsASN, ednsASName string
		if err := rows.Scan(&rec.ID, &rec.Domain, &rec.QueryType, &rec.ClientIP,
			&rec.CountryCode, &rec.City, &rec.Subnet, &rec.EDNSCountryCode, &rec.EDNSCity, &ednsASN, &ednsASName, &rec.NSID, &rec.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning EDNS history: %w", err)
		}
		rec.EDNSASN = ednsASN
		rec.EDNSASName = ednsASName
		items = append(items, &rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating EDNS history: %w", err)
	}

	return &EDNSResult{Total: int(total), Items: items}, nil
}

// DeleteEDNS deletes EDNS records matching the given filters.
// Only the edns table is affected; the corresponding queries records remain.
// Empty filters delete all EDNS records.
func (r *Recorder) DeleteEDNS(id int64, subnet, countryCode, nsid, start, end string) (int64, error) {
	where := "1=1"
	args := make([]interface{}, 0)
	ph := 0

	if id > 0 {
		ph++
		where += fmt.Sprintf(" AND id = $%d", ph)
		args = append(args, id)
	}
	if subnet != "" {
		ph++
		where += fmt.Sprintf(" AND subnet = $%d", ph)
		args = append(args, subnet)
	}
	if countryCode != "" {
		ph++
		where += fmt.Sprintf(" AND country_code = $%d", ph)
		args = append(args, countryCode)
	}
	if nsid != "" {
		ph++
		where += fmt.Sprintf(" AND nsid = $%d", ph)
		args = append(args, nsid)
	}
	if start != "" {
		ph++
		where += fmt.Sprintf(" AND created_at >= $%d", ph)
		args = append(args, r.timeFilter(start))
	}
	if end != "" {
		ph++
		where += fmt.Sprintf(" AND created_at <= $%d", ph)
		args = append(args, r.timeFilter(end))
	}

	delQuery := fmt.Sprintf("DELETE FROM edns WHERE %s", where)
	res, err := r.db.Exec(delQuery, args...)
	if err != nil {
		return 0, fmt.Errorf("deleting edns: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DeleteQueries deletes query history (and associated edns records) matching the
// given filters. Empty filters delete everything.
func (r *Recorder) DeleteQueries(id int64, domain, clientIP, subnet, countryCode, start, end string) (int64, error) {
	where := "1=1"
	args := make([]interface{}, 0)
	ph := 0

	if id > 0 {
		ph++
		where += fmt.Sprintf(" AND id = $%d", ph)
		args = append(args, id)
	}
	if domain != "" {
		ph++
		where += fmt.Sprintf(" AND domain = $%d", ph)
		args = append(args, domain)
	}
	if clientIP != "" {
		ph++
		where += fmt.Sprintf(" AND client_ip = $%d", ph)
		args = append(args, clientIP)
	}
	if subnet != "" {
		ph++
		if r.dbType == "sqlite" {
			where += fmt.Sprintf(" AND ip_in_subnet(client_ip, $%d)", ph)
		} else {
			// <<= : client_ip is contained by or equals the given subnet
			where += fmt.Sprintf(" AND client_ip <<= $%d", ph)
		}
		args = append(args, subnet)
	}
	if countryCode != "" {
		ph++
		where += fmt.Sprintf(" AND country_code = $%d", ph)
		args = append(args, countryCode)
	}
	if start != "" {
		ph++
		where += fmt.Sprintf(" AND created_at >= $%d", ph)
		args = append(args, r.timeFilter(start))
	}
	if end != "" {
		ph++
		where += fmt.Sprintf(" AND created_at <= $%d", ph)
		args = append(args, r.timeFilter(end))
	}

	tx, err := r.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	delEDNS := fmt.Sprintf("DELETE FROM edns WHERE id IN (SELECT id FROM queries WHERE %s)", where)
	if _, err := tx.Exec(delEDNS, args...); err != nil {
		return 0, fmt.Errorf("deleting edns: %w", err)
	}

	delQ := fmt.Sprintf("DELETE FROM queries WHERE %s", where)
	res, err := tx.Exec(delQ, args...)
	if err != nil {
		return 0, fmt.Errorf("deleting queries: %w", err)
	}
	n, _ := res.RowsAffected()

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return n, nil
}

// DeleteBefore deletes records older than the given time.
func (r *Recorder) DeleteBefore(before string) (int64, error) {
	before = r.timeFilter(before)
	tx, err := r.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM edns WHERE id IN (SELECT id FROM queries WHERE created_at < $1)", before); err != nil {
		return 0, fmt.Errorf("deleting edns: %w", err)
	}

	res, err := tx.Exec("DELETE FROM queries WHERE created_at < $1", before)
	if err != nil {
		return 0, fmt.Errorf("deleting queries: %w", err)
	}
	n, _ := res.RowsAffected()

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return n, nil
}

// collect reads entries from r.ch and dispatches them to the worker pool
// via r.taskCh. On stop it drains remaining entries and closes taskCh.
func (r *Recorder) collect() {
	defer r.wg.Done()

	for {
		select {
		case <-r.stop:
			// Drain any remaining entries then signal workers to exit.
			for {
				select {
				case e := <-r.ch:
					r.taskCh <- e
				default:
					close(r.taskCh)
					return
				}
			}
		case e := <-r.ch:
			r.taskCh <- e
		}
	}
}

// worker reads entries from taskCh and performs a direct per-entry insert.
// Each insert runs in its own implicit transaction — no batching — for minimal
// latency.
func (r *Recorder) worker() {
	defer r.wg.Done()

	for e := range r.taskCh {
		r.insertOne(e)
	}
}

// insertOne writes one entry and queues a geo retry when needed. PostgreSQL
// uses a CTE for atomic EDNS inserts; SQLite uses a transaction.
func (r *Recorder) insertOne(e *Entry) {
	var err error
	var newID int64

	if r.dbType == "sqlite" {
		newID, err = r.insertOneSQLite(e)
	} else if e.EDNSSubnet != "" {
		// CTE: single SQL statement, zero extra network round-trips, fully atomic.
		err = r.db.QueryRow(`
			WITH new_query AS (
				INSERT INTO queries (domain, query_type, client_ip, country_code, city, geo_cached, edns_included, asn, as_name, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, TRUE, $7, $8, $9)
				RETURNING id
			)
			INSERT INTO edns (id, subnet, country_code, city, asn, as_name, nsid)
			SELECT id, $10, $11, $12, $13, $14, $15 FROM new_query
			RETURNING id`,
			e.Domain, e.QueryType, e.ClientIP, e.CountryCode, e.City, e.GeoCached, e.ASN, e.ASName, e.Timestamp,
			e.EDNSSubnet, e.EDNSCountryCode, e.EDNSCity, e.EDNSASN, e.EDNSASName, e.NSID,
		).Scan(&newID)
	} else {
		err = r.db.QueryRow(
			`INSERT INTO queries (domain, query_type, client_ip, country_code, city, geo_cached, edns_included, asn, as_name, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, FALSE, $7, $8, $9)
			 RETURNING id`,
			e.Domain, e.QueryType, e.ClientIP, e.CountryCode, e.City, e.GeoCached, e.ASN, e.ASName, e.Timestamp,
		).Scan(&newID)
	}

	if err != nil {
		dropped := atomic.AddInt64(&r.dropped, 1)
		// Log the first failure and then every 100th drop to avoid log storms.
		if dropped == 1 || dropped%100 == 0 {
			log.Printf("[recorder] insert failed (dropped=%d): %v", dropped, err)
		}
		return
	}

	// Queue geo retry if the entry had failed geo lookup.
	if e.GeoFailed && r.geo != nil && newID > 0 {
		job := &geoRetryJob{QueryID: newID, ClientIP: e.ClientIP, EDNSSubnet: e.EDNSSubnet}
		select {
		case r.retryCh <- job:
		default:
			// Retry queue full; will be retried on next restart.
		}
	}
}

// geoRetryWorker reads retry jobs from retryCh, re-does geo lookups, and
// updates the queries/edns tables in the background.
func (r *Recorder) geoRetryWorker() {
	defer r.wg.Done()

	for {
		select {
		case <-r.stop:
			return
		case job := <-r.retryCh:
			r.retryGeo(job)
		}
	}
}

// retryGeo re-does geo lookup for a previously-failed entry and updates the DB.
// Retries up to 3 times with 30s backoff.
func (r *Recorder) retryGeo(job *geoRetryJob) {
	if r.geo == nil {
		return
	}

	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(30 * time.Second)
			select {
			case <-timer.C:
			case <-r.stop:
				timer.Stop()
				return
			}
		}

		cc, city, asn, asName, _, err := r.geo.Lookup(job.ClientIP)
		if err != nil {
			continue
		}

		_, err = r.db.Exec(
			`UPDATE queries SET country_code=$1, city=$2, asn=$3, as_name=$4 WHERE id=$5`,
			cc, city, asn, asName, job.QueryID,
		)
		if err != nil {
			log.Printf("[recorder] geo retry: update queries id=%d failed: %v", job.QueryID, err)
			continue
		}

		// Also update edns table if EDNS subnet was present.
		if job.EDNSSubnet != "" {
			ecsIP := subnetFirstIPFromCIDR(job.EDNSSubnet)
			if ecsIP != "" {
				ecsCC, ecsCity, ecsASN, ecsASName, _, ecsErr := r.geo.Lookup(ecsIP)
				if ecsErr == nil {
					_, _ = r.db.Exec(
						`UPDATE edns SET country_code=$1, city=$2, asn=$3, as_name=$4 WHERE id=$5`,
						ecsCC, ecsCity, ecsASN, ecsASName, job.QueryID,
					)
				}
			}
		}

		log.Printf("[recorder] geo retry ok: id=%d cc=%s city=%s asn=%s as=%s", job.QueryID, cc, city, asn, asName)
		return
	}

	log.Printf("[recorder] geo retry exhausted: id=%d ip=%s", job.QueryID, job.ClientIP)
}

// subnetFirstIPFromCIDR returns the masked IPv4 or IPv6 network address.
func subnetFirstIPFromCIDR(cidr string) string {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}
	return network.IP.String()
}
