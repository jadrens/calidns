package recorder

import (
	"database/sql"
	"net"
	"net/url"
	"sync"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
)

const sqliteTimeLayout = "2006-01-02T15:04:05.000000000Z"

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS queries (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	domain TEXT NOT NULL,
	query_type TEXT NOT NULL,
	client_ip TEXT NOT NULL DEFAULT '0.0.0.0',
	country_code TEXT NOT NULL DEFAULT 'NO',
	city TEXT NOT NULL DEFAULT 'empty',
	geo_cached INTEGER NOT NULL DEFAULT 0,
	edns_included INTEGER NOT NULL DEFAULT 0,
	asn TEXT,
	as_name TEXT NOT NULL DEFAULT 'empty',
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_domain ON queries(domain);
CREATE INDEX IF NOT EXISTS idx_created_at ON queries(created_at);

CREATE TABLE IF NOT EXISTS edns (
	id INTEGER PRIMARY KEY,
	subnet TEXT,
	country_code TEXT NOT NULL DEFAULT 'NO',
	city TEXT NOT NULL DEFAULT 'empty',
	asn TEXT,
	as_name TEXT NOT NULL DEFAULT 'empty',
	nsid TEXT NOT NULL DEFAULT 'empty',
	created_at TEXT NOT NULL
);
`

var registerRecorderSQLiteOnce sync.Once

func openSQLite(path string) (*sql.DB, error) {
	registerRecorderSQLiteOnce.Do(func() {
		sql.Register("sqlite3_recorder", &sqlite3.SQLiteDriver{
			ConnectHook: func(conn *sqlite3.SQLiteConn) error {
				return conn.RegisterFunc("ip_in_subnet", func(ip, subnet string) bool {
					parsedIP := net.ParseIP(ip)
					_, network, err := net.ParseCIDR(subnet)
					return err == nil && parsedIP != nil && network.Contains(parsedIP)
				}, true)
			},
		})
	})
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("_journal_mode", "WAL")
	q.Set("_synchronous", "NORMAL")
	q.Set("_busy_timeout", "5000")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite3_recorder", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func (r *Recorder) timeFilter(value string) string {
	if r.dbType != "sqlite" || value == "" {
		return value
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return value
	}
	return parsed.UTC().Format(sqliteTimeLayout)
}

func (r *Recorder) insertOneSQLite(e *Entry) (int64, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	createdAt := e.Timestamp.UTC().Format(sqliteTimeLayout)
	result, err := tx.Exec(`
		INSERT INTO queries (domain, query_type, client_ip, country_code, city, geo_cached, edns_included, asn, as_name, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		e.Domain, e.QueryType, e.ClientIP, e.CountryCode, e.City, e.GeoCached, e.EDNSSubnet != "", e.ASN, e.ASName, createdAt)
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if e.EDNSSubnet != "" {
		_, err = tx.Exec(`
			INSERT INTO edns (id, subnet, country_code, city, asn, as_name, nsid, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			id, e.EDNSSubnet, e.EDNSCountryCode, e.EDNSCity, e.EDNSASN, e.EDNSASName, e.NSID, createdAt)
		if err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}
