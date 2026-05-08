// Package detlog persists a rolling log of detector events to a SQLite
// database stored alongside the binary. Each row records which species were
// detected in a single inference tick, the top confidence, and, when the
// detector triggered an auto-capture, the name of the resulting picture file.
package detlog

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Entry struct {
	ID            int64     `json:"id"`
	Timestamp     time.Time `json:"timestamp"`
	Classes       []string  `json:"classes"`
	TopClass      string    `json:"top_class"`
	TopConfidence float64   `json:"top_confidence"`
	Picture       string    `json:"picture,omitempty"`
}

type Logger struct {
	db *sql.DB
}

func Open(path string) (*Logger, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open log db: %w", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS detections (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts TIMESTAMP NOT NULL,
		classes TEXT NOT NULL,
		top_class TEXT NOT NULL,
		top_confidence REAL NOT NULL,
		picture TEXT
	)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create table: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_detections_ts ON detections(ts DESC)`); err != nil {
		db.Close()
		return nil, err
	}
	return &Logger{db: db}, nil
}

func (l *Logger) Close() error { return l.db.Close() }

func (l *Logger) Append(entry Entry) (int64, error) {
	classes := strings.Join(entry.Classes, ",")
	var pic any
	if entry.Picture != "" {
		pic = entry.Picture
	}
	res, err := l.db.Exec(
		`INSERT INTO detections (ts, classes, top_class, top_confidence, picture) VALUES (?, ?, ?, ?, ?)`,
		entry.Timestamp.UTC(), classes, entry.TopClass, entry.TopConfidence, pic,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AttachPicture sets the picture filename on an existing detection row.
func (l *Logger) AttachPicture(id int64, picture string) error {
	_, err := l.db.Exec(`UPDATE detections SET picture = ? WHERE id = ?`, picture, id)
	return err
}

// PurgeOlderThan deletes detection rows whose timestamp is older than
// now-age. Returns the number of rows removed.
func (l *Logger) PurgeOlderThan(age time.Duration) (int64, error) {
	cutoff := time.Now().Add(-age).UTC()
	res, err := l.db.Exec(`DELETE FROM detections WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (l *Logger) List(limit int, beforeID int64) ([]Entry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var (
		rows *sql.Rows
		err  error
	)
	if beforeID > 0 {
		rows, err = l.db.Query(
			`SELECT id, ts, classes, top_class, top_confidence, picture
			 FROM detections WHERE id < ? ORDER BY id DESC LIMIT ?`,
			beforeID, limit)
	} else {
		rows, err = l.db.Query(
			`SELECT id, ts, classes, top_class, top_confidence, picture
			 FROM detections ORDER BY id DESC LIMIT ?`,
			limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Entry, 0, limit)
	for rows.Next() {
		var (
			e       Entry
			classes string
			picture sql.NullString
		)
		if err := rows.Scan(&e.ID, &e.Timestamp, &classes, &e.TopClass, &e.TopConfidence, &picture); err != nil {
			return nil, err
		}
		if classes != "" {
			e.Classes = strings.Split(classes, ",")
		}
		if picture.Valid {
			e.Picture = picture.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
