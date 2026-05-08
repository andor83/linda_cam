# Linda_Cam

A single-binary 4K RTSP camera viewer with embedded Vue UI, HLS live stream
(NVENC-accelerated when a GPU is available, libx264 on CPU otherwise),
manual snapshots, automatic YOLOv8 animal detection, and a 526-class bird
species classifier.

## Quick start (Docker)

The container ships everything needed: Go binary, ffmpeg (with both NVENC and
libx264), ONNX Runtime, and pre-exported YOLOv8 + bird-classifier ONNX
models. Persistent state lives in `~/.birdcam/` on the host.

```bash
git clone <this-repo> linda_cam && cd linda_cam

# CPU-only (works on any host):
docker compose up -d --build

# With NVIDIA GPU (h264_nvenc, requires nvidia-container-toolkit):
docker compose -f docker-compose.yml -f docker-compose.gpu.yml up -d --build
```

Then open http://localhost:8080. First run prompts for a password; after
logging in, paste your camera's RTSP URL into **Settings**, click **Save**,
and open **Live**.

To force one mode regardless of host detection:

```bash
LINDA_HWACCEL=cuda docker compose up -d   # require GPU
LINDA_HWACCEL=none docker compose up -d   # never use GPU even if one is present
```

### Persistent storage layout (`~/.birdcam/`)

| Path | Purpose |
| --- | --- |
| `config.json`            | Server config (RTSP URL, hashed password, detection settings, eBird/AI keys). Created on first run. |
| `log.db`                 | SQLite detection log — every detector tick that saw a watched animal. |
| `sightings.db`           | SQLite sightings store + cropped-thumbnail index. |
| `models/yolov8n.onnx`    | YOLOv8 detector (Open Images V7 export, 600 classes). Seeded from the image on first run; replace freely. |
| `models/bird_classifier.onnx` + `bird_classifier_classes.json` | 526-class bird species ViT classifier. Seeded from the image on first run. |
| `pictures/`              | Captured JPEGs (manual + auto-capture). |
| `hls/`                   | HLS playlist + segments. Transient — recreated on every start. |

Drop in your own ONNX models by replacing the files in `~/.birdcam/models/`
and restarting the container — the entrypoint only seeds defaults if the
target file is missing.

## Settings (web UI / `config.json` keys)

Most of these are exposed in the **Settings** tab. The full set lives in
`internal/config/config.go`.

| Key | Default | What it does |
| --- | --- | --- |
| `rtsp_url`                  | (empty) | RTSP source URL. Use `rtsp://user:pass@host:554/path`. |
| `http_addr`                 | `:8080` | Address the HTTP server binds to. |
| `password_hash`             | (empty) | bcrypt hash of the login password. Set on first run via the UI. |
| `session_key`               | (random) | HMAC key for session cookies. Generated on first run. |
| `session_timeout_s`         | 30 days | Session lifetime in seconds. |
| `detection_cooldown_s`      | 5       | Minimum seconds between auto-saved detection JPEGs. |
| `auto_capture_enabled`      | false   | When true, watched-species detections above threshold are auto-saved. |
| `watched_animals`           | []      | List of `{name, threshold}` — species names from the model's class list and per-species confidence cutoffs (0–1). |
| `bird_confidence_threshold` | 0.6     | Confidence cutoff for the bird-specialist classifier. |
| `bird_max_crops`            | 4       | Max bird crops per detection tick to feed to the classifier. |
| `classifier_corrections`    | []      | List of `{detected, correction, regex}` rules to rewrite classifier output (e.g. fix common misidentifications). |
| `ai_quality`                | (off)   | OpenAI-compatible vision-LLM gate that scores capture quality and discards weak frames. Fields: `enabled`, `url`, `model`, `bearer_token`, `discard_threshold` (0–100), `normalize_width`, `max_candidates`. |
| `ebird`                     | (off)   | eBird integration for "is this species plausible at my location?" hints. Fields: `enabled`, `api_key`, `region` (e.g. `US-PA-101`) or `lat`/`lng` + `dist_km`, `back_days`. |

## Features

- **Live view** — HLS stream of H.264 video served from
  `/api/live/stream.m3u8` via hls.js (Safari uses native HLS). NVENC encode
  on GPU hosts (`preset p4`, ~5 Mb/s VBR) or libx264 on CPU
  (`preset veryfast`, `crf 23`). Audio is AAC passthrough or transcoded to
  MP3 if AAC copy fails. Latency ~4–6 s.
- **Manual capture** — saves the latest frame as a JPEG to `pictures/`.
- **Animal detection** — YOLOv8 ONNX runs once per second against the
  latest frame. Class list is read from the model's metadata; any
  ultralytics export works.
- **Bird species classifier** — when the detector sees a bird, top-N crops
  are passed to a 526-class species classifier. Optional per-species
  correction rules and confidence thresholds.
- **Optional AI quality gate** — pass each candidate frame through an
  OpenAI-compatible vision LLM and discard low-scoring ones before storage.
- **Optional eBird integration** — flags species not plausibly observed in
  your region/date range.
- **Gallery** — thumbnails of every capture, newest first, with species
  tags on auto-captures.
- **Detection log** — every watched-species tick is appended to `log.db`
  with timestamp, classes, top confidence, and saved-picture filename.
- **Auth** — single password (bcrypt-hashed) and HMAC-signed session
  cookie; sessions last 30 days by default.

## Running outside Docker

You'll need: Go 1.25+, Node 20+, system ffmpeg (with NVENC if you want GPU),
ONNX Runtime, and Python with `ultralytics` + `transformers` + `torch` for
first-run model exports.

```bash
make deps-web        # first time only: npm install
make web             # build Vue SPA
make build           # compile linda_cam
make fetch-onnxruntime
make fetch-ffmpeg    # optional: static ffmpeg fallback into ./bin/
./launch.sh          # exports models on first run, probes NVENC, exec's the binary
```

`launch.sh` requires `h264_nvenc` ffmpeg. For a CPU-only bare-metal run,
either use the Docker container or set `LINDA_HWACCEL=none` and adapt
`launch.sh` to skip the NVENC requirement.

## Endpoints (HTTP API)

- `GET /api/session` — public; `{authenticated, first_run}`
- `POST /api/login` — `{password}`; sets session cookie
- `POST /api/first-run` — `{password}`; only valid when no password is set
- `POST /api/logout`
- `GET/PUT /api/config`
- `POST /api/test-rtsp` — `{rtsp_url}`; tries to pull one frame
- `POST /api/capture` — saves the latest frame
- `GET /api/pictures` / `GET /api/pictures/{name}` (`?download=1`) / `DELETE /api/pictures/{name}`
- `GET /api/snapshot.jpg` — latest frame
- `GET /api/status` — `animals_present: [{class_id, name, confidence}]`
- `GET /api/classes` — model class names
- `GET /api/detections?limit=100&before=<id>` — detection log
- `GET /api/live/stream.m3u8` and `GET /api/live/{init.mp4|seg_NNNNN.m4s}`

## Repo layout

```
cmd/linda_cam/      Go entrypoint
cmd/recompress/     Off-line tool to recompress old captures
internal/           Application packages (auth, capture, classifier, detector,
                    httpapi, rtsp, sightings, stream, web, ...)
schema/             SQLite schemas (log.db, sightings.db)
tools/              Python helpers (bird-classifier ONNX export)
web/                Vue 3 + Vite source; built into internal/web/dist/
Dockerfile          4-stage build: web → models → Go binary → runtime
docker-compose.yml  CPU-default compose
docker-compose.gpu.yml  Overlay adding NVIDIA reservation + LINDA_HWACCEL=cuda
docker/entrypoint.sh    Container entrypoint: seeds models, picks ffmpeg, probes RTSP
launch.sh           Bare-metal launch script (NVENC required)
```
