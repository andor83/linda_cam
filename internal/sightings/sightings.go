// Package sightings persists a row per saved picture (jpg) in SQLite.
// This is the system's source of truth for picture metadata: the same
// fields previously kept in `<picture>.meta.json` sidecars, plus a
// `hearted` flag (favourited pictures aren't culled by retention) and
// a `picture_deleted` flag (set when the .jpg has been pruned by the
// 30-day sweep but the row stays so statistics survive).
//
// Detections and the bird-species guess list live in TEXT columns as
// JSON blobs — they're cheap to scan, and we never need to query into
// them from SQL.
package sightings

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/linda/linda_cam/internal/capture"
)

// Store wraps the sightings.db connection. Methods are safe for
// concurrent use (sql.DB is connection-pooled).
type Store struct {
	db *sql.DB
}

// Open creates/opens the database, applies migrations, and returns a
// ready-to-use Store.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sightings db: %w", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS sightings (
		name TEXT PRIMARY KEY,
		saved_at TIMESTAMP NOT NULL,
		analyzed_at TIMESTAMP,
		reclassified_at TIMESTAMP,
		user_species TEXT NOT NULL DEFAULT '',
		user_notes TEXT NOT NULL DEFAULT '',
		canonical_species TEXT NOT NULL DEFAULT '',
		detections_json TEXT NOT NULL DEFAULT '',
		bird_species_json TEXT NOT NULL DEFAULT '',
		bird_crop TEXT NOT NULL DEFAULT '',
		ai_quality_score INTEGER,
		ai_quality_at TIMESTAMP,
		ai_quality_error TEXT NOT NULL DEFAULT '',
		ai_quality_raw TEXT NOT NULL DEFAULT '',
		crops_json TEXT NOT NULL DEFAULT '',
		hearted INTEGER NOT NULL DEFAULT 0,
		picture_deleted INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create table: %w", err)
	}
	// Tolerant migrations for DBs created before these columns existed.
	// SQLite has no `ADD COLUMN IF NOT EXISTS`; duplicate-column is the
	// only acceptable failure here.
	for _, alter := range []string{
		`ALTER TABLE sightings ADD COLUMN ai_quality_error TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sightings ADD COLUMN ai_quality_raw TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sightings ADD COLUMN crops_json TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.Exec(alter); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	for _, idx := range []string{
		`CREATE INDEX IF NOT EXISTS idx_sightings_saved_at ON sightings(saved_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_sightings_canonical ON sightings(canonical_species)`,
		`CREATE INDEX IF NOT EXISTS idx_sightings_hearted ON sightings(hearted)`,
		`CREATE INDEX IF NOT EXISTS idx_sightings_picture_deleted ON sightings(picture_deleted)`,
	} {
		if _, err := db.Exec(idx); err != nil {
			db.Close()
			return nil, fmt.Errorf("index: %w", err)
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Insert creates a fresh row for a newly-saved picture. Idempotent: a
// re-save under an existing name is treated as a no-op (preserves
// whatever metadata may have been written between save + analyze).
func (s *Store) Insert(name string, savedAt time.Time) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO sightings (name, saved_at) VALUES (?, ?)`,
		name, savedAt.UTC(),
	)
	return err
}

// Upsert writes the metadata fields for an existing row (or creates
// the row if it doesn't exist yet — useful for backfill). savedAt is
// only applied when inserting.
func (s *Store) Upsert(name string, savedAt time.Time, md capture.Metadata) error {
	canonical := canonicalSpecies(md)
	dets, err := encodeJSON(md.Detections)
	if err != nil {
		return err
	}
	birds, err := encodeJSON(md.BirdSpecies)
	if err != nil {
		return err
	}
	crops, err := encodeJSON(md.BirdCrops)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO sightings
		  (name, saved_at, analyzed_at, reclassified_at,
		   user_species, user_notes, canonical_species,
		   detections_json, bird_species_json, bird_crop,
		   ai_quality_score, ai_quality_at, ai_quality_error,
		   ai_quality_raw, crops_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
		  analyzed_at      = excluded.analyzed_at,
		  reclassified_at  = excluded.reclassified_at,
		  user_species     = excluded.user_species,
		  user_notes       = excluded.user_notes,
		  canonical_species= excluded.canonical_species,
		  detections_json  = excluded.detections_json,
		  bird_species_json= excluded.bird_species_json,
		  bird_crop        = excluded.bird_crop,
		  ai_quality_score = excluded.ai_quality_score,
		  ai_quality_at    = excluded.ai_quality_at,
		  ai_quality_error = excluded.ai_quality_error,
		  ai_quality_raw   = excluded.ai_quality_raw,
		  crops_json       = excluded.crops_json`,
		name, savedAt.UTC(),
		nullableTime(md.AnalyzedAt), nullableTime(md.ReclassifiedAt),
		md.UserSpecies, md.UserNotes, canonical,
		dets, birds, md.BirdCrop,
		nullableInt(md.AIQualityScore), nullableTime(md.AIQualityAt),
		md.AIQualityError, md.AIQualityRaw,
		crops,
	)
	return err
}

// Get returns the metadata for one row. The bool is false (with no
// error) when the row doesn't exist.
func (s *Store) Get(name string) (capture.Metadata, bool, error) {
	row := s.db.QueryRow(`
		SELECT analyzed_at, reclassified_at, user_species, user_notes,
		       detections_json, bird_species_json, bird_crop,
		       ai_quality_score, ai_quality_at, ai_quality_error,
		       ai_quality_raw, crops_json
		FROM sightings WHERE name = ?`, name)
	var (
		md      capture.Metadata
		ana, re sql.NullTime
		dets    string
		birds   string
		score   sql.NullInt64
		scoreAt sql.NullTime
		crops   string
	)
	err := row.Scan(&ana, &re, &md.UserSpecies, &md.UserNotes,
		&dets, &birds, &md.BirdCrop, &score, &scoreAt,
		&md.AIQualityError, &md.AIQualityRaw, &crops)
	if errors.Is(err, sql.ErrNoRows) {
		return capture.Metadata{}, false, nil
	}
	if err != nil {
		return capture.Metadata{}, false, err
	}
	if ana.Valid {
		t := ana.Time
		md.AnalyzedAt = &t
	}
	if re.Valid {
		t := re.Time
		md.ReclassifiedAt = &t
	}
	if score.Valid {
		v := int(score.Int64)
		md.AIQualityScore = &v
	}
	if scoreAt.Valid {
		t := scoreAt.Time
		md.AIQualityAt = &t
	}
	if dets != "" {
		_ = json.Unmarshal([]byte(dets), &md.Detections)
	}
	if birds != "" {
		_ = json.Unmarshal([]byte(birds), &md.BirdSpecies)
	}
	if crops != "" {
		_ = json.Unmarshal([]byte(crops), &md.BirdCrops)
	}
	// Legacy fallback: pre-migration rows have a single bird_crop +
	// bird_species_json and no crops_json. Synthesize a one-element
	// BirdCrops view in memory so downstream consumers can treat
	// every row uniformly without caring about schema vintage.
	if len(md.BirdCrops) == 0 && md.BirdCrop != "" {
		md.BirdCrops = []capture.BirdCropInfo{{
			Filename: md.BirdCrop,
			Species:  md.BirdSpecies,
		}}
	}
	return md, true, nil
}

// List returns one Picture per row whose .jpg is still on disk
// (picture_deleted = 0), newest first. Caller is responsible for
// supplying the pictures directory so file size + crop existence can
// be enriched via stat.
func (s *Store) List(picturesDir string) ([]capture.Picture, error) {
	rows, err := s.db.Query(`
		SELECT name, saved_at, analyzed_at, reclassified_at,
		       user_species, user_notes,
		       detections_json, bird_species_json, bird_crop,
		       ai_quality_score, ai_quality_at, ai_quality_error,
		       crops_json, hearted
		FROM sightings
		WHERE picture_deleted = 0
		ORDER BY saved_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]capture.Picture, 0, 256)
	for rows.Next() {
		var (
			p          capture.Picture
			savedAt    time.Time
			analyzed   sql.NullTime
			reclass    sql.NullTime
			dets, brds string
			score      sql.NullInt64
			scoreAt    sql.NullTime
			crops      string
			hearted    int
		)
		if err := rows.Scan(&p.Name, &savedAt, &analyzed, &reclass,
			&p.UserSpecies, &p.UserNotes, &dets, &brds, &p.BirdCrop,
			&score, &scoreAt, &p.AIQualityError, &crops, &hearted); err != nil {
			return nil, err
		}
		p.ModTime = savedAt
		p.Species = speciesFromName(p.Name)
		p.Manual = strings.Contains(p.Name, "_manual")
		p.Hearted = hearted == 1
		if analyzed.Valid {
			t := analyzed.Time
			p.AnalyzedAt = &t
		}
		if reclass.Valid {
			t := reclass.Time
			p.ReclassifiedAt = &t
		}
		if score.Valid {
			v := int(score.Int64)
			p.AIQualityScore = &v
		}
		if scoreAt.Valid {
			t := scoreAt.Time
			p.AIQualityAt = &t
		}
		if dets != "" {
			_ = json.Unmarshal([]byte(dets), &p.Detections)
		}
		if brds != "" {
			_ = json.Unmarshal([]byte(brds), &p.BirdSpecies)
		}
		if crops != "" {
			_ = json.Unmarshal([]byte(crops), &p.BirdCrops)
		}
		// Legacy single-crop rows synthesize a one-element BirdCrops
		// in memory so the frontend always sees the same shape.
		if len(p.BirdCrops) == 0 && p.BirdCrop != "" {
			p.BirdCrops = []capture.BirdCropInfo{{
				Filename: p.BirdCrop,
				Species:  p.BirdSpecies,
			}}
		}
		// Stat the source file for size + presence; skip rows whose
		// jpg vanished out from under us (fs/db drift).
		info, err := os.Stat(filepath.Join(picturesDir, p.Name))
		if err != nil {
			continue
		}
		p.Size = info.Size()
		if p.BirdCrop != "" {
			if _, err := os.Stat(filepath.Join(picturesDir, p.BirdCrop)); err == nil {
				p.HasCrop = true
			}
		}
		// has_crop also true when at least one of the new multi-crop
		// files exists on disk. Cheap lookup since the json arrived
		// in the same row.
		if !p.HasCrop {
			for _, c := range p.BirdCrops {
				if c.Filename == "" {
					continue
				}
				if _, err := os.Stat(filepath.Join(picturesDir, c.Filename)); err == nil {
					p.HasCrop = true
					break
				}
			}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Delete removes the row entirely. Used for manual user deletes; the
// retention cull uses MarkPictureDeleted instead.
func (s *Store) Delete(name string) error {
	_, err := s.db.Exec(`DELETE FROM sightings WHERE name = ?`, name)
	return err
}

// AllNames returns every row's name (including picture_deleted=1).
// Used by bulk operations like apply-corrections that need to touch
// every metadata row regardless of whether the .jpg still exists on
// disk — corrections to the canonical species name keep historical
// statistics accurate even for culled pictures.
func (s *Store) AllNames() ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM sightings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// SetHeart updates only the hearted flag.
func (s *Store) SetHeart(name string, hearted bool) error {
	v := 0
	if hearted {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE sightings SET hearted = ? WHERE name = ?`, v, name)
	return err
}

// Hearted reads the flag for one row. Returns false (no error) if the
// row doesn't exist.
func (s *Store) Hearted(name string) (bool, error) {
	var v int
	err := s.db.QueryRow(`SELECT hearted FROM sightings WHERE name = ?`, name).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v == 1, nil
}

// MarkPictureDeleted is what the retention cull calls instead of
// Delete: the .jpg is gone but the row stays so statistics keep
// counting historic sightings.
func (s *Store) MarkPictureDeleted(name string) error {
	_, err := s.db.Exec(`UPDATE sightings SET picture_deleted = 1 WHERE name = ?`, name)
	return err
}

// CullCandidates returns names of non-hearted pictures whose saved_at
// is older than cutoff and whose .jpg hasn't already been pruned. The
// caller deletes the file artifacts and then calls MarkPictureDeleted.
func (s *Store) CullCandidates(cutoff time.Time) ([]string, error) {
	rows, err := s.db.Query(`
		SELECT name FROM sightings
		WHERE saved_at < ? AND hearted = 0 AND picture_deleted = 0`,
		cutoff.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// StatRow is one row pulled by StatRowsSince — what stats packages
// need to bucket sightings by species and time.
type StatRow struct {
	Name     string
	Species  string
	When     time.Time // analyzed_at, falling back to saved_at
	Hearted  bool
}

// StatRowsSince returns one row per picture whose effective time
// (analyzed_at when set, else saved_at) is at or after `since`.
// Pictures with no canonical species are excluded.
//
// We select analyzed_at and saved_at as separate columns (rather than
// COALESCE'ing them in SQL) because the modernc/sqlite driver can't
// scan the generic-text result of a COALESCE back into *time.Time;
// the typed columns scan cleanly into sql.NullTime / time.Time.
func (s *Store) StatRowsSince(since time.Time) ([]StatRow, error) {
	rows, err := s.db.Query(`
		SELECT name, canonical_species, analyzed_at, saved_at, hearted
		FROM sightings
		WHERE canonical_species != ''
		  AND (
		    (analyzed_at IS NOT NULL AND analyzed_at >= ?)
		    OR (analyzed_at IS NULL AND saved_at >= ?)
		  )`,
		since.UTC(), since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatRow
	for rows.Next() {
		var (
			r        StatRow
			analyzed sql.NullTime
			savedAt  time.Time
			hearted  int
		)
		if err := rows.Scan(&r.Name, &r.Species, &analyzed, &savedAt, &hearted); err != nil {
			return nil, err
		}
		when := savedAt
		if analyzed.Valid {
			when = analyzed.Time
		}
		r.When = when.Local()
		r.Hearted = hearted == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// Totals captures the counts shown in the Statistics tab's KPI row.
type Totals struct {
	Pictures       int // live + culled rows that still have a species
	SightingsToday int
	Sightings7d    int
	Species30d     int
}

func (s *Store) Totals(now time.Time) (Totals, error) {
	loc := now.Location()
	startToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).UTC()
	cutoff7d := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -6).UTC()
	cutoff30d := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -29).UTC()

	var t Totals
	if err := s.db.QueryRow(`
		SELECT COUNT(*) FROM sightings WHERE canonical_species != ''`,
	).Scan(&t.Pictures); err != nil {
		return t, err
	}
	if err := s.db.QueryRow(`
		SELECT COUNT(*) FROM sightings
		WHERE canonical_species != ''
		  AND COALESCE(analyzed_at, saved_at) >= ?`, startToday,
	).Scan(&t.SightingsToday); err != nil {
		return t, err
	}
	if err := s.db.QueryRow(`
		SELECT COUNT(*) FROM sightings
		WHERE canonical_species != ''
		  AND COALESCE(analyzed_at, saved_at) >= ?`, cutoff7d,
	).Scan(&t.Sightings7d); err != nil {
		return t, err
	}
	if err := s.db.QueryRow(`
		SELECT COUNT(DISTINCT canonical_species) FROM sightings
		WHERE canonical_species != ''
		  AND COALESCE(analyzed_at, saved_at) >= ?`, cutoff30d,
	).Scan(&t.Species30d); err != nil {
		return t, err
	}
	return t, nil
}

// canonicalSpecies picks the name to GROUP BY in stats: user
// override wins, else the top classifier guess.
func canonicalSpecies(md capture.Metadata) string {
	if s := strings.TrimSpace(md.UserSpecies); s != "" {
		return s
	}
	// Prefer the highest-confidence species from the multi-crop set
	// (the new bird pipeline). Fall back to the legacy single-crop
	// BirdSpecies for pre-migration sightings.
	bestName := ""
	bestConf := 0.0
	for _, c := range md.BirdCrops {
		if len(c.Species) == 0 {
			continue
		}
		if c.Species[0].Confidence > bestConf {
			bestConf = c.Species[0].Confidence
			bestName = strings.TrimSpace(c.Species[0].Name)
		}
	}
	if bestName != "" {
		return bestName
	}
	if len(md.BirdSpecies) > 0 {
		if s := strings.TrimSpace(md.BirdSpecies[0].Name); s != "" {
			return s
		}
	}
	return ""
}

func encodeJSON(v any) (string, error) {
	switch t := v.(type) {
	case []capture.DetectionRecord:
		if len(t) == 0 {
			return "", nil
		}
	case []capture.SpeciesGuess:
		if len(t) == 0 {
			return "", nil
		}
	case []capture.BirdCropInfo:
		if len(t) == 0 {
			return "", nil
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	if string(b) == "null" {
		return "", nil
	}
	return string(b), nil
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

func nullableInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

// speciesFromName mirrors capture.speciesFromName so List() can
// derive the species token without round-tripping through the capture
// package's unexported helper. Kept in sync manually.
func speciesFromName(name string) string {
	n := strings.TrimSuffix(strings.ToLower(name), ".jpg")
	parts := strings.Split(n, "_")
	if len(parts) != 4 {
		return ""
	}
	sp := parts[2]
	if sp == "manual" || sp == "auto" {
		return ""
	}
	return sp
}

// Backfill walks the pictures directory once and upserts a row for
// every .jpg + sidecar pair not already in the database. Returns the
// number of rows inserted/updated.
//
// Existing rows are NOT touched (the SQL row is the source of truth
// going forward; sidecar files are only consulted on first launch).
// Rows with a different .jpg presence are reconciled — picture_deleted
// flips back to 0 if the file reappears (manual file copy).
func (s *Store) Backfill(picturesDir string) (int, error) {
	entries, err := os.ReadDir(picturesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	have := map[string]bool{}
	{
		rows, err := s.db.Query(`SELECT name FROM sightings`)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err == nil {
				have[n] = true
			}
		}
		rows.Close()
	}
	added := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		lower := strings.ToLower(name)
		if !strings.HasSuffix(lower, ".jpg") {
			continue
		}
		// Skip every crop sidecar — both the legacy `.crop.jpg` and the
		// new indexed `.crop.<n>.jpg` produced by the bird pipeline.
		// They live in the pictures dir alongside the source frames
		// but should never become top-level gallery entries.
		if strings.Contains(name, ".crop.") {
			continue
		}
		if have[name] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		md := readSidecar(filepath.Join(picturesDir, name+".meta.json"))
		savedAt := info.ModTime()
		if md.AnalyzedAt != nil && md.AnalyzedAt.Before(savedAt) {
			savedAt = *md.AnalyzedAt
		}
		if err := s.Upsert(name, savedAt, md); err != nil {
			return added, err
		}
		added++
	}
	return added, nil
}

func readSidecar(path string) capture.Metadata {
	body, err := os.ReadFile(path)
	if err != nil {
		return capture.Metadata{}
	}
	var md capture.Metadata
	_ = json.Unmarshal(body, &md)
	return md
}

// PurgeCropRows removes any sightings rows whose `name` matches a
// crop sidecar pattern (`*.crop.*.jpg`). A pre-fix Backfill mistakenly
// indexed multi-crop files as top-level pictures; this cleans them up.
// Returns the number of rows deleted. Idempotent.
func (s *Store) PurgeCropRows() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM sightings WHERE name LIKE '%.crop.%.jpg' OR name LIKE '%.crop.jpg'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PurgeOrphanThumbsForCrops deletes thumbnail files whose name is a
// crop filename (e.g. `<picture>.jpg.crop.0.jpg`). The pre-fix
// pipeline generated thumbnails for those bogus rows; once their DB
// entries are gone we want their cached thumbs gone too. Returns the
// number of files removed.
func PurgeOrphanCropThumbs(picturesDir string) (int, error) {
	thumbDir := filepath.Join(picturesDir, ".thumbs")
	entries, err := os.ReadDir(thumbDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.Contains(name, ".crop.") {
			continue
		}
		if err := os.Remove(filepath.Join(thumbDir, name)); err == nil {
			removed++
		}
	}
	return removed, nil
}

// RecoverFromSidecars walks legacy `<name>.meta.json` files and
// restores SQL fields that an earlier destructive reclassify run
// emptied. For each sidecar with content, we re-populate:
//   • bird_species_json (from sidecar's BirdSpecies) when the SQL is empty
//   • ai_quality_score / ai_quality_at when the SQL is null
//   • bird_crop pointer (only if the file still exists on disk)
//   • detections_json (from sidecar's Detections) when the SQL is empty
//   • analyzed_at (when the SQL is null)
//
// Never overwrites populated fields — user_species, hearted, and
// crops_json (the new pipeline's data) are fully preserved. Returns
// the number of rows touched.
func (s *Store) RecoverFromSidecars(picturesDir string) (int, error) {
	entries, err := os.ReadDir(picturesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	recovered := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".meta.json") {
			continue
		}
		sourceName := strings.TrimSuffix(name, ".meta.json")
		// Read sidecar.
		body, err := os.ReadFile(filepath.Join(picturesDir, name))
		if err != nil {
			continue
		}
		var side capture.Metadata
		if err := json.Unmarshal(body, &side); err != nil {
			continue
		}
		// Read current SQL state.
		md, ok, err := s.Get(sourceName)
		if err != nil || !ok {
			continue
		}
		dirty := false
		// bird_species
		if len(md.BirdSpecies) == 0 && len(side.BirdSpecies) > 0 {
			md.BirdSpecies = side.BirdSpecies
			dirty = true
		}
		// detections
		if len(md.Detections) == 0 && len(side.Detections) > 0 {
			md.Detections = side.Detections
			dirty = true
		}
		// bird_crop pointer (only if file exists)
		if md.BirdCrop == "" && side.BirdCrop != "" {
			if _, err := os.Stat(filepath.Join(picturesDir, side.BirdCrop)); err == nil {
				md.BirdCrop = side.BirdCrop
				dirty = true
			}
		}
		// AI quality score
		if md.AIQualityScore == nil && side.AIQualityScore != nil {
			s := *side.AIQualityScore
			md.AIQualityScore = &s
			if side.AIQualityAt != nil {
				t := *side.AIQualityAt
				md.AIQualityAt = &t
			}
			dirty = true
		}
		// analyzed_at
		if md.AnalyzedAt == nil && side.AnalyzedAt != nil {
			t := *side.AnalyzedAt
			md.AnalyzedAt = &t
			dirty = true
		}
		if !dirty {
			continue
		}
		// Use Upsert to write back. savedAt is only consulted on
		// INSERT; existing rows preserve their saved_at.
		if err := s.Upsert(sourceName, time.Now(), md); err != nil {
			continue
		}
		recovered++
	}
	return recovered, nil
}

// Names returns every row's name (used by the orphan-cleanup pass to
// identify rows whose .jpg vanished, so picture_deleted gets flipped
// without waiting for the cull).
func (s *Store) ReconcilePictureDeleted(picturesDir string) (int, error) {
	rows, err := s.db.Query(`SELECT name FROM sightings WHERE picture_deleted = 0`)
	if err != nil {
		return 0, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err == nil {
			names = append(names, n)
		}
	}
	rows.Close()
	flipped := 0
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(picturesDir, n)); errors.Is(err, os.ErrNotExist) {
			if err := s.MarkPictureDeleted(n); err == nil {
				flipped++
			}
		}
	}
	return flipped, nil
}
