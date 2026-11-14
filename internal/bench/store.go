package bench

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

var storeIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Store keeps run records and operator controls in the existing WAL database.
type Store struct {
	Root string
	db   *sql.DB
	mu   sync.Mutex
}

func NewStore(root string) (*Store, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(absolute, "bench.sqlite3"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{Root: absolute, db: db}
	if _, err := db.Exec(`PRAGMA busy_timeout=10000`); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		_ = db.Close()
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS runs (id TEXT PRIMARY KEY, result TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS events (seq INTEGER PRIMARY KEY, run_id TEXT, event TEXT);
		CREATE TABLE IF NOT EXISTS questions (id TEXT PRIMARY KEY, run_id TEXT, question TEXT,
			metadata TEXT, answer TEXT, created REAL);`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) RunDir(id string) (string, error) {
	if !storeIdentifierPattern.MatchString(id) {
		return "", fmt.Errorf("Invalid run identifier")
	}
	runs := filepath.Join(s.Root, "runs")
	run := filepath.Join(runs, id)
	for _, path := range []string{runs, run} {
		info, err := os.Lstat(path)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("Artifact directories may not be symlinks")
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	if err := os.MkdirAll(run, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(run)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("Artifact directories may not be symlinks")
	}
	return run, nil
}

func marshalStoreJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func (s *Store) Save(result Record) error {
	encoded, err := marshalStoreJSON(result)
	if err != nil {
		return err
	}
	id := text(result["id"])
	s.mu.Lock()
	defer s.mu.Unlock()
	runDir, err := s.RunDir(id)
	if err != nil {
		return err
	}
	path := filepath.Join(runDir, "result.json")
	temp := filepath.Join(runDir, "result.tmp")
	if err := os.WriteFile(temp, []byte(encoded+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO runs VALUES (?, ?) ON CONFLICT(id) DO UPDATE SET result=excluded.result`, id, encoded)
	return err
}

func decodeStoreRecord(encoded string) (Record, error) {
	var result Record
	if err := decodeJSON([]byte(encoded), &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) Get(id string) (Record, error) {
	var encoded string
	err := s.db.QueryRow(`SELECT result FROM runs WHERE id=?`, id).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("run %q not found", id)
	}
	if err != nil {
		return nil, err
	}
	return decodeStoreRecord(encoded)
}

func (s *Store) List() ([]Record, error) {
	rows, err := s.db.Query(`SELECT result FROM runs ORDER BY rowid DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Record{}
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return nil, err
		}
		record, err := decodeStoreRecord(encoded)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (s *Store) Event(runID string, event Record) error {
	encodedRecord := Record{"timestamp": unixTime()}
	for key, value := range event {
		encodedRecord[key] = value
	}
	encoded, err := marshalStoreJSON(encodedRecord)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	transaction, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err = transaction.Exec(`INSERT INTO events(run_id,event) VALUES (?,?)`, runID, encoded); err != nil {
		_ = transaction.Rollback()
		return err
	}
	runDir, err := s.RunDir(runID)
	if err != nil {
		_ = transaction.Rollback()
		return err
	}
	file, err := os.OpenFile(filepath.Join(runDir, "transcript.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		_ = transaction.Rollback()
		return err
	}
	_, writeErr := file.WriteString(encoded + "\n")
	closeErr := file.Close()
	if writeErr != nil {
		_ = transaction.Rollback()
		return writeErr
	}
	if closeErr != nil {
		_ = transaction.Rollback()
		return closeErr
	}
	return transaction.Commit()
}

func (s *Store) Events(runID string) ([]Record, error) {
	rows, err := s.db.Query(`SELECT event FROM events WHERE run_id=? ORDER BY seq DESC LIMIT 500`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	reversed := []Record{}
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return nil, err
		}
		var event Record
		if err := decodeJSON([]byte(encoded), &event); err != nil {
			return nil, err
		}
		reversed = append(reversed, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	return reversed, nil
}

func (s *Store) Question(runID, question string, metadata Record) (string, error) {
	encoded, err := marshalStoreJSON(metadata)
	if err != nil {
		return "", err
	}
	id := newID()
	_, err = s.db.Exec(`INSERT INTO questions VALUES (?,?,?,?,NULL,?)`, id, runID, question, encoded, unixTime())
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) Questions(runID string) ([]Record, error) {
	query := `SELECT id,run_id,question,metadata FROM questions WHERE answer IS NULL`
	args := []any{}
	if runID != "" {
		query += ` AND run_id=?`
		args = append(args, runID)
	}
	rows, err := s.db.Query(query+` ORDER BY rowid`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Record{}
	for rows.Next() {
		var id, run, question, metadata string
		if err := rows.Scan(&id, &run, &question, &metadata); err != nil {
			return nil, err
		}
		var decoded any
		if err := decodeJSON([]byte(metadata), &decoded); err != nil {
			return nil, err
		}
		result = append(result, Record{"id": id, "run_id": run, "question": question, "metadata": decoded})
	}
	return result, rows.Err()
}

func (s *Store) Response(runID, questionID string) (any, bool, error) {
	var answer sql.NullString
	err := s.db.QueryRow(`SELECT answer FROM questions WHERE run_id=? AND id=?`, runID, questionID).Scan(&answer)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !answer.Valid {
		return nil, true, nil
	}
	return answer.String, true, nil
}

func (s *Store) Answer(runID, questionID string, answer any) (bool, error) {
	value, ok := answer.(string)
	if !ok || strings.TrimSpace(value) == "" {
		return false, fmt.Errorf("Answer must not be empty")
	}
	result, err := s.db.Exec(`UPDATE questions SET answer=? WHERE run_id=? AND id=? AND answer IS NULL`, value, runID, questionID)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func (s *Store) Interventions(runID string) (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM questions WHERE run_id=? AND answer IS NOT NULL`, runID).Scan(&count)
	return count, err
}

func (s *Store) Stop(runID string) error {
	if _, err := s.Get(runID); err != nil {
		return err
	}
	runDir, err := s.RunDir(runID)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(runDir, "stop-requested"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return file.Close()
}

func (s *Store) StopRequested(runID string) (bool, error) {
	runDir, err := s.RunDir(runID)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(filepath.Join(runDir, "stop-requested"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
