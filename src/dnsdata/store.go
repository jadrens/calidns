// Package dnsdata persists Web-API-managed DNS zones and defaults separately
// from the human-edited YAML configuration.
package dnsdata

import (
	"database/sql"
	"encoding/json"
	"fmt"

	_ "github.com/mattn/go-sqlite3"

	"calidns/src/config"
)

const (
	defaultTTL      = 300
	defaultRecord   = false
	defaultResponse = "refuse"
)

// Defaults are DNS behaviours that can be changed through the Web API.
type Defaults struct {
	TTL      int
	Record   bool
	Response string
}

// Store is the local dns_data.db database.
type Store struct{ db *sql.DB }

// Open creates the schema and inserts default settings on first use.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_synchronous=NORMAL&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("opening DNS data: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.initialize(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) initialize() error {
	const schema = `
CREATE TABLE IF NOT EXISTS dns_defaults (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  default_ttl INTEGER NOT NULL,
  default_record INTEGER NOT NULL,
  default_response TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS dns_zones (
  pattern TEXT PRIMARY KEY,
  data TEXT NOT NULL,
  position INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_dns_zones_position ON dns_zones(position);
INSERT OR IGNORE INTO dns_defaults(id, default_ttl, default_record, default_response)
VALUES (1, 300, 0, 'refuse');`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("initializing DNS data: %w", err)
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) LoadDefaults() (Defaults, error) {
	d := Defaults{TTL: defaultTTL, Record: defaultRecord, Response: defaultResponse}
	err := s.db.QueryRow(`SELECT default_ttl, default_record, default_response
FROM dns_defaults WHERE id = 1`).Scan(&d.TTL, &d.Record, &d.Response)
	if err != nil {
		return d, fmt.Errorf("loading DNS defaults: %w", err)
	}
	return d, nil
}

func (s *Store) SaveDefaults(d Defaults) error {
	_, err := s.db.Exec(`UPDATE dns_defaults
SET default_ttl = ?, default_record = ?, default_response = ? WHERE id = 1`,
		d.TTL, d.Record, d.Response)
	if err != nil {
		return fmt.Errorf("saving DNS defaults: %w", err)
	}
	return nil
}

func (s *Store) LoadZones() (map[string]*config.ZoneConfig, error) {
	rows, err := s.db.Query("SELECT pattern, data FROM dns_zones ORDER BY position, rowid")
	if err != nil {
		return nil, fmt.Errorf("loading DNS zones: %w", err)
	}
	defer rows.Close()
	zones := make(map[string]*config.ZoneConfig)
	for rows.Next() {
		var pattern string
		var data []byte
		if err := rows.Scan(&pattern, &data); err != nil {
			return nil, err
		}
		var zone config.ZoneConfig
		if err := json.Unmarshal(data, &zone); err != nil {
			return nil, fmt.Errorf("decoding DNS zone %q: %w", pattern, err)
		}
		zones[pattern] = &zone
	}
	return zones, rows.Err()
}

func (s *Store) UpsertZone(pattern string, zone *config.ZoneConfig) error {
	data, err := json.Marshal(zone)
	if err != nil {
		return fmt.Errorf("encoding DNS zone %q: %w", pattern, err)
	}
	_, err = s.db.Exec(`INSERT INTO dns_zones(pattern, data, position)
VALUES(?, ?, COALESCE((SELECT MAX(position) + 1 FROM dns_zones), 0))
ON CONFLICT(pattern) DO UPDATE SET data=excluded.data`, pattern, data)
	if err != nil {
		return fmt.Errorf("saving DNS zone %q: %w", pattern, err)
	}
	return nil
}

func (s *Store) DeleteZone(pattern string) error {
	_, err := s.db.Exec("DELETE FROM dns_zones WHERE pattern = ?", pattern)
	if err != nil {
		return fmt.Errorf("deleting DNS zone %q: %w", pattern, err)
	}
	return nil
}

// ReplaceZones atomically replaces all zones, used by initial cluster sync.
func (s *Store) ReplaceZones(zones map[string]*config.ZoneConfig) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM dns_zones"); err != nil {
		return err
	}
	position := 0
	for pattern, zone := range zones {
		data, err := json.Marshal(zone)
		if err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO dns_zones(pattern, data, position) VALUES(?, ?, ?)", pattern, data, position); err != nil {
			return err
		}
		position++
	}
	return tx.Commit()
}
