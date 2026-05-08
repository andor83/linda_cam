-- log.db: detector event log. Mirrors internal/detlog/detlog.go Open().
PRAGMA journal_mode = WAL;

CREATE TABLE IF NOT EXISTS detections (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ts TIMESTAMP NOT NULL,
    classes TEXT NOT NULL,
    top_class TEXT NOT NULL,
    top_confidence REAL NOT NULL,
    picture TEXT
);

CREATE INDEX IF NOT EXISTS idx_detections_ts ON detections(ts DESC);
