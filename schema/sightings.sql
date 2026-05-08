-- sightings.db: per-picture metadata. Mirrors internal/sightings/sightings.go Open().
-- The CREATE TABLE here already includes columns the Go code adds via tolerant
-- ALTER TABLE migrations (ai_quality_error, ai_quality_raw, crops_json), so a
-- fresh DB never needs the migration path.
PRAGMA journal_mode = WAL;

CREATE TABLE IF NOT EXISTS sightings (
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
);

CREATE INDEX IF NOT EXISTS idx_sightings_saved_at ON sightings(saved_at DESC);
CREATE INDEX IF NOT EXISTS idx_sightings_canonical ON sightings(canonical_species);
CREATE INDEX IF NOT EXISTS idx_sightings_hearted ON sightings(hearted);
CREATE INDEX IF NOT EXISTS idx_sightings_picture_deleted ON sightings(picture_deleted);
