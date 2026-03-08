package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"strconv"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"

	"llm-router/config"
)

const sessionTTL = 24 * time.Hour

type Store struct {
	db *sql.DB
}

type AppConfig struct {
	OllamaBaseURL        string
	ClassifierModel      string
	ThinkingModel        string
	CodingModel          string
	SimpleModel          string
	DefaultModel         string
	ClassificationPrompt string
	ClassifierTimeoutS   int
	CacheTTLS            int
	CacheMaxSize         int
}

type APIKey struct {
	ID         int64
	Name       string
	KeyPreview string
	Active     bool
	CreatedAt  time.Time
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
}

type RequestRecord struct {
	ID             int64
	Timestamp      time.Time
	Classification string
	Model          string
	Prompt         string
	LatencyMS      int64
	StatusCode     int
	CacheHit       bool
}

type Stats struct {
	Total            int64
	AvgLatencyMS     float64
	ByClassification  map[string]int64
	ByModel           map[string]int64
	CacheHitsByModel  map[string]int64
}

func New(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	db.SetMaxOpenConns(1)

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		return nil, fmt.Errorf("set busy_timeout: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS config (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS api_keys (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			name         TEXT    NOT NULL,
			key_hash     TEXT    NOT NULL UNIQUE,
			key_preview  TEXT    NOT NULL,
			active       INTEGER NOT NULL DEFAULT 1,
			created_at   DATETIME NOT NULL,
			last_used_at DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS requests (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp      DATETIME NOT NULL,
			classification TEXT     NOT NULL,
			model          TEXT     NOT NULL,
			prompt         TEXT     NOT NULL,
			latency_ms     INTEGER  NOT NULL,
			status_code    INTEGER  NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS users (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			username      TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			token      TEXT PRIMARY KEY,
			expires_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS login_attempts (
			key        TEXT NOT NULL,
			attempts   INTEGER NOT NULL DEFAULT 0,
			locked_until DATETIME,
			updated_at DATETIME NOT NULL,
			PRIMARY KEY (key)
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate exec: %w", err)
		}
	}
	// Add cache_hit column for existing databases (ignore error if already exists)
	s.db.Exec(`ALTER TABLE requests ADD COLUMN cache_hit INTEGER NOT NULL DEFAULT 0`)
	// Add expires_at column for existing databases (ignore error if already exists)
	s.db.Exec(`ALTER TABLE api_keys ADD COLUMN expires_at DATETIME`)
	// Add indices for commonly queried columns (idempotent)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_requests_timestamp ON requests(timestamp)`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_requests_model ON requests(model)`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_requests_classification ON requests(classification)`)
	return nil
}

// SeedDefaults populates config from cfg for any keys not yet present.
func (s *Store) SeedDefaults(cfg *config.Config) error {
	defaults := map[string]string{
		"ollama_base_url":        cfg.OllamaBaseURL,
		"classifier_model":       cfg.ClassifierModel,
		"thinking_model":         cfg.ThinkingModel,
		"coding_model":           cfg.CodingModel,
		"simple_model":           cfg.SimpleModel,
		"default_model":          cfg.DefaultModel,
		"classification_prompt":  cfg.ClassificationPrompt,
		"classifier_timeout_s":   strconv.Itoa(cfg.ClassifierTimeoutS),
		"cache_ttl_s":            strconv.Itoa(cfg.CacheTTLS),
		"cache_max_size":         strconv.Itoa(cfg.CacheMaxSize),
	}
	for k, v := range defaults {
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO config (key, value) VALUES (?, ?)`, k, v); err != nil {
			return fmt.Errorf("seed config %s: %w", k, err)
		}
	}
	return nil
}

// SeedUser creates the admin user if they don't already exist.
func (s *Store) SeedUser(username, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT OR IGNORE INTO users (username, password_hash) VALUES (?, ?)`, username, string(hash))
	return err
}

func (s *Store) AuthenticateUser(username, password string) (bool, error) {
	var hash string
	err := s.db.QueryRow(`SELECT password_hash FROM users WHERE username = ?`, username).Scan(&hash)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil, nil
}

// GetConfig reads all config values from the DB.
func (s *Store) GetConfig() (AppConfig, error) {
	rows, err := s.db.Query(`SELECT key, value FROM config`)
	if err != nil {
		return AppConfig{}, err
	}
	defer rows.Close()

	m := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return AppConfig{}, err
		}
		m[k] = v
	}

	return AppConfig{
		OllamaBaseURL:        m["ollama_base_url"],
		ClassifierModel:      m["classifier_model"],
		ThinkingModel:        m["thinking_model"],
		CodingModel:          m["coding_model"],
		SimpleModel:          m["simple_model"],
		DefaultModel:         m["default_model"],
		ClassificationPrompt: m["classification_prompt"],
		ClassifierTimeoutS:   parseInt(m["classifier_timeout_s"], 10),
		CacheTTLS:            parseInt(m["cache_ttl_s"], 300),
		CacheMaxSize:         parseInt(m["cache_max_size"], 500),
	}, nil
}

func parseInt(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return def
}

func (s *Store) SetConfigValue(key, value string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO config (key, value) VALUES (?, ?)`, key, value)
	return err
}

// API key management

func (s *Store) CreateAPIKey(name string, expiryDays int) (rawKey string, err error) {
	b := make([]byte, 24)
	if _, err = rand.Read(b); err != nil {
		return "", err
	}
	rawKey = "llmr_" + hex.EncodeToString(b)
	hash := sha256.Sum256([]byte(rawKey))
	keyHash := hex.EncodeToString(hash[:])
	preview := rawKey[:13] // "llmr_" + first 8 hex chars

	var expiresAt any
	if expiryDays > 0 {
		t := time.Now().Add(time.Duration(expiryDays) * 24 * time.Hour)
		expiresAt = t
	}

	_, err = s.db.Exec(
		`INSERT INTO api_keys (name, key_hash, key_preview, active, created_at, expires_at) VALUES (?, ?, ?, 1, ?, ?)`,
		name, keyHash, preview, time.Now(), expiresAt,
	)
	return rawKey, err
}

// HasActiveKeys returns true if at least one active API key exists.
func (s *Store) HasActiveKeys() (bool, error) {
	var count int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE active = 1`).Scan(&count)
	return count > 0, err
}

// ValidateAPIKey checks a raw key against stored hashes and updates last_used_at.
func (s *Store) ValidateAPIKey(rawKey string) (bool, error) {
	hash := sha256.Sum256([]byte(rawKey))
	keyHash := hex.EncodeToString(hash[:])

	var id int64
	var active int64
	var expiresAt sql.NullTime
	err := s.db.QueryRow(`SELECT id, active, expires_at FROM api_keys WHERE key_hash = ?`, keyHash).Scan(&id, &active, &expiresAt)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if active != 1 {
		return false, nil
	}
	if expiresAt.Valid && time.Now().After(expiresAt.Time) {
		return false, nil
	}

	if _, err := s.db.Exec(`UPDATE api_keys SET last_used_at = ? WHERE id = ?`, time.Now(), id); err != nil {
		log.Printf("update last_used_at: %v", err)
	}
	return true, nil
}

func (s *Store) ListAPIKeys() ([]APIKey, error) {
	rows, err := s.db.Query(
		`SELECT id, name, key_preview, active, created_at, last_used_at, expires_at FROM api_keys ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []APIKey
	for rows.Next() {
		var k APIKey
		var active int64
		var lastUsed sql.NullTime
		var expiresAt sql.NullTime
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyPreview, &active, &k.CreatedAt, &lastUsed, &expiresAt); err != nil {
			return nil, err
		}
		k.Active = active == 1
		if lastUsed.Valid {
			k.LastUsedAt = &lastUsed.Time
		}
		if expiresAt.Valid {
			k.ExpiresAt = &expiresAt.Time
		}
		keys = append(keys, k)
	}
	return keys, nil
}

func (s *Store) RevokeAPIKey(id int64) error {
	_, err := s.db.Exec(`UPDATE api_keys SET active = 0 WHERE id = ?`, id)
	return err
}

// Request tracking

func (s *Store) RecordRequest(classification, model, prompt string, latencyMS int64, statusCode int, cacheHit bool) error {
	if len(prompt) > 300 {
		prompt = prompt[:300] + "…"
	}
	var cacheHitInt int
	if cacheHit {
		cacheHitInt = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO requests (timestamp, classification, model, prompt, latency_ms, status_code, cache_hit) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		time.Now(), classification, model, prompt, latencyMS, statusCode, cacheHitInt,
	)
	return err
}

func (s *Store) ListRequests(limit, offset int) ([]RequestRecord, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, classification, model, prompt, latency_ms, status_code, cache_hit FROM requests ORDER BY timestamp DESC LIMIT ? OFFSET ?`,
		limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []RequestRecord
	for rows.Next() {
		var r RequestRecord
		var cacheHit int64
		if err := rows.Scan(&r.ID, &r.Timestamp, &r.Classification, &r.Model, &r.Prompt, &r.LatencyMS, &r.StatusCode, &cacheHit); err != nil {
			return nil, err
		}
		r.CacheHit = cacheHit == 1
		records = append(records, r)
	}
	return records, nil
}

func (s *Store) CountRequests() (int64, error) {
	var count int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&count)
	return count, err
}

func (s *Store) GetStats(since time.Time) (Stats, error) {
	stats := Stats{
		ByClassification: make(map[string]int64),
		ByModel:          make(map[string]int64),
		CacheHitsByModel: make(map[string]int64),
	}

	if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(AVG(latency_ms), 0) FROM requests WHERE timestamp >= ?`, since).
		Scan(&stats.Total, &stats.AvgLatencyMS); err != nil {
		return stats, err
	}

	rows, err := s.db.Query(`SELECT classification, COUNT(*) FROM requests WHERE timestamp >= ? GROUP BY classification`, since)
	if err != nil {
		return stats, err
	}
	defer rows.Close()
	for rows.Next() {
		var class string
		var count int64
		if err := rows.Scan(&class, &count); err != nil {
			return stats, err
		}
		stats.ByClassification[class] = count
	}

	rows2, err := s.db.Query(`SELECT model, COUNT(*) FROM requests WHERE timestamp >= ? GROUP BY model`, since)
	if err != nil {
		return stats, err
	}
	defer rows2.Close()
	for rows2.Next() {
		var model string
		var count int64
		if err := rows2.Scan(&model, &count); err != nil {
			return stats, err
		}
		stats.ByModel[model] = count
	}

	rows3, err := s.db.Query(`SELECT model, COUNT(*) FROM requests WHERE cache_hit = 1 AND timestamp >= ? GROUP BY model`, since)
	if err != nil {
		return stats, err
	}
	defer rows3.Close()
	for rows3.Next() {
		var model string
		var count int64
		if err := rows3.Scan(&model, &count); err != nil {
			return stats, err
		}
		stats.CacheHitsByModel[model] = count
	}

	return stats, nil
}

// Session management (persisted to SQLite)

func (s *Store) CreateSession() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	_, err := s.db.Exec(`INSERT INTO sessions (token, expires_at) VALUES (?, ?)`, token, time.Now().Add(sessionTTL))
	return token, err
}

func (s *Store) ValidateSession(token string) bool {
	var expiresAt time.Time
	err := s.db.QueryRow(`SELECT expires_at FROM sessions WHERE token = ?`, token).Scan(&expiresAt)
	if err != nil {
		return false
	}
	if time.Now().After(expiresAt) {
		s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
		return false
	}
	return true
}

func (s *Store) DeleteSession(token string) {
	s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
}

// CleanupSessions removes all expired sessions from the DB.
func (s *Store) CleanupSessions() {
	if res, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now()); err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			log.Printf("cleaned up %d expired sessions", n)
		}
	}
}

// RequestFilter holds optional filter criteria for request listing.
type RequestFilter struct {
	Search         string
	Classification string
	Model          string
}

func (s *Store) ListRequestsFiltered(limit, offset int, f RequestFilter) ([]RequestRecord, error) {
	q := `SELECT id, timestamp, classification, model, prompt, latency_ms, status_code, cache_hit FROM requests WHERE 1=1`
	args := []any{}
	if f.Search != "" {
		q += ` AND prompt LIKE ?`
		args = append(args, "%"+f.Search+"%")
	}
	if f.Classification != "" {
		q += ` AND classification = ?`
		args = append(args, f.Classification)
	}
	if f.Model != "" {
		q += ` AND model = ?`
		args = append(args, f.Model)
	}
	q += ` ORDER BY timestamp DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []RequestRecord
	for rows.Next() {
		var r RequestRecord
		var cacheHit int64
		if err := rows.Scan(&r.ID, &r.Timestamp, &r.Classification, &r.Model, &r.Prompt, &r.LatencyMS, &r.StatusCode, &cacheHit); err != nil {
			return nil, err
		}
		r.CacheHit = cacheHit == 1
		records = append(records, r)
	}
	return records, nil
}

func (s *Store) CountRequestsFiltered(f RequestFilter) (int64, error) {
	q := `SELECT COUNT(*) FROM requests WHERE 1=1`
	args := []any{}
	if f.Search != "" {
		q += ` AND prompt LIKE ?`
		args = append(args, "%"+f.Search+"%")
	}
	if f.Classification != "" {
		q += ` AND classification = ?`
		args = append(args, f.Classification)
	}
	if f.Model != "" {
		q += ` AND model = ?`
		args = append(args, f.Model)
	}
	var count int64
	err := s.db.QueryRow(q, args...).Scan(&count)
	return count, err
}

// AllRequests returns all records (no pagination) for export purposes.
func (s *Store) AllRequests(f RequestFilter) ([]RequestRecord, error) {
	return s.ListRequestsFiltered(1<<31-1, 0, f)
}

// DistinctModels returns all distinct model names from the requests table.
func (s *Store) DistinctModels() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT model FROM requests ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var models []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		models = append(models, m)
	}
	return models, nil
}

const (
	maxLoginAttempts  = 5
	lockoutDuration   = 15 * time.Minute
)

// IsLoginLocked returns true if the given key (username or IP) is currently locked out.
func (s *Store) IsLoginLocked(key string) (bool, error) {
	var lockedUntil sql.NullTime
	err := s.db.QueryRow(`SELECT locked_until FROM login_attempts WHERE key = ?`, key).Scan(&lockedUntil)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if lockedUntil.Valid && time.Now().Before(lockedUntil.Time) {
		return true, nil
	}
	return false, nil
}

// RecordLoginFailure increments the failure counter for key. Returns true if the account is now locked.
func (s *Store) RecordLoginFailure(key string) (locked bool, err error) {
	now := time.Now()
	// Upsert attempt count
	_, err = s.db.Exec(`
		INSERT INTO login_attempts (key, attempts, locked_until, updated_at)
		VALUES (?, 1, NULL, ?)
		ON CONFLICT(key) DO UPDATE SET
			attempts = CASE
				WHEN locked_until IS NOT NULL AND locked_until < ? THEN 1
				ELSE attempts + 1
			END,
			locked_until = CASE
				WHEN locked_until IS NOT NULL AND locked_until < ? THEN NULL
				ELSE locked_until
			END,
			updated_at = ?
	`, key, now, now, now, now)
	if err != nil {
		return false, err
	}

	var attempts int
	if err := s.db.QueryRow(`SELECT attempts FROM login_attempts WHERE key = ?`, key).Scan(&attempts); err != nil {
		return false, err
	}

	if attempts >= maxLoginAttempts {
		lockedUntil := now.Add(lockoutDuration)
		_, err = s.db.Exec(`UPDATE login_attempts SET locked_until = ?, attempts = 0 WHERE key = ?`, lockedUntil, key)
		return true, err
	}
	return false, nil
}

// ClearLoginFailures resets the counter for key on successful login.
func (s *Store) ClearLoginFailures(key string) {
	s.db.Exec(`DELETE FROM login_attempts WHERE key = ?`, key)
}
