#!/usr/bin/env bash
# Container entrypoint for linda_cam. Mirrors launch.sh but works against
# /data (the bind-mounted volume) for all mutable state, with the static
# app living read-only at $LINDA_APP_DIR.
set -euo pipefail

APP="${LINDA_APP_DIR:-/opt/linda_cam}"
DATA="${LINDA_DATA_DIR:-/data}"

log() { printf '[entrypoint] %s\n' "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

mkdir -p "$DATA" "$DATA/models" "$DATA/pictures" "$DATA/hls"

# ---- 0. Seed the data volume on first run ----------------------------------

# Models: copy the pre-exported defaults if the user hasn't supplied their own.
for f in yolov8n.onnx bird_classifier.onnx bird_classifier_classes.json; do
    src="$APP/models-default/$f"
    dst="$DATA/models/$f"
    if [[ ! -s "$dst" && -s "$src" ]]; then
        log "seed models/$f from image"
        cp "$src" "$dst"
    fi
done

# Databases: apply schema if absent.
init_db() {
    local db="$1" sql="$2"
    [[ -s "$db" ]] && return 0
    [[ -r "$sql" ]] || die "schema file missing: $sql"
    log "init $(basename "$db") from $(basename "$sql")"
    sqlite3 "$db" < "$sql"
}
init_db "$DATA/log.db"       "$APP/schema/log.sql"
init_db "$DATA/sightings.db" "$APP/schema/sightings.sql"

# ---- 1. Pick ffmpeg --------------------------------------------------------
# Prefer system ffmpeg (Ubuntu repo). h264_nvenc is used when an NVIDIA GPU is
# attached; otherwise fall back to libx264 on CPU. LINDA_HWACCEL forwards the
# decision to the Go binary's stream package.

pick_ffmpeg() {
    local require_nvenc="$1"
    local candidates=(/usr/bin/ffmpeg /usr/local/bin/ffmpeg "$APP/bin/ffmpeg-static")
    for c in "${candidates[@]}"; do
        [[ -x "$c" ]] || continue
        if [[ "$require_nvenc" == "1" ]]; then
            local enc
            enc=$("$c" -hide_banner -encoders 2>/dev/null) || continue
            [[ "$enc" == *h264_nvenc* ]] || continue
        fi
        printf '%s' "$c"; return 0
    done
    return 1
}

# LINDA_HWACCEL can be set by the user to force a mode. Empty = autodetect.
HW="${LINDA_HWACCEL:-}"

if [[ "$HW" == "none" ]]; then
    FF=$(pick_ffmpeg 0) || die "no ffmpeg found in image"
    log "ffmpeg=$FF (LINDA_HWACCEL=none, libx264)"
elif [[ "$HW" == "cuda" ]]; then
    FF=$(pick_ffmpeg 1) || die "LINDA_HWACCEL=cuda but no ffmpeg with h264_nvenc found"
    log "ffmpeg=$FF (LINDA_HWACCEL=cuda)"
elif FF=$(pick_ffmpeg 1); then
    HW="cuda"
    log "ffmpeg=$FF (h264_nvenc available)"
elif FF=$(pick_ffmpeg 0); then
    HW="none"
    log "ffmpeg=$FF (no h264_nvenc — using libx264 on CPU)"
else
    die "no ffmpeg found in image"
fi

# ---- 2. Probe RTSP if configured -------------------------------------------

RTSP=""
if [[ -r "$DATA/config.json" ]]; then
    RTSP="$(jq -r '.rtsp_url // empty' "$DATA/config.json" 2>/dev/null || true)"
fi

PROBE_DIR="$(mktemp -d -t linda-probe-XXXXXX)"
trap 'rm -rf "$PROBE_DIR"' EXIT

probe() {
    local audio="$1" rc=0 stderr_log="$PROBE_DIR/ffmpeg.err"
    local -a args=(-hide_banner -loglevel error)
    if [[ "$HW" == "cuda" ]]; then
        args+=(-hwaccel cuda -hwaccel_output_format cuda)
    fi
    args+=(-rtsp_transport tcp -i "$RTSP" -t 3)
    if [[ "$HW" == "cuda" ]]; then
        args+=(-c:v h264_nvenc -preset p4 -rc vbr -cq 23 -b:v 5M -maxrate 8M -g 30)
    else
        args+=(-c:v libx264 -preset veryfast -tune zerolatency -crf 23 -maxrate 5M -bufsize 10M -g 30 -pix_fmt yuv420p)
    fi
    if [[ "$audio" == "copy" ]]; then args+=(-c:a copy)
    else                              args+=(-c:a libmp3lame -b:a 128k); fi
    args+=(
        -f hls -hls_time 2 -hls_list_size 6
        -hls_flags delete_segments+append_list+omit_endlist
        -hls_segment_type fmp4
        -hls_segment_filename "$PROBE_DIR/seg_%05d.m4s"
        "$PROBE_DIR/probe.m3u8"
    )
    rm -f "$PROBE_DIR"/* 2>/dev/null || true
    "$FF" "${args[@]}" 2>"$stderr_log" || rc=$?
    if [[ $rc -ne 0 ]]; then
        log "probe audio=$audio failed (rc=$rc)"
        sed 's/^/[ffmpeg] /' "$stderr_log" >&2 || true
        return $rc
    fi
    compgen -G "$PROBE_DIR/seg_*.m4s" >/dev/null || { log "probe audio=$audio produced no segments"; return 1; }
    return 0
}

AUDIO_MODE="copy"
if [[ -z "$RTSP" ]]; then
    log "no rtsp_url configured yet — skipping probe; set it in Settings, then restart"
else
    log "rtsp_url resolved ($(printf '%s' "$RTSP" | sed -E 's#(://[^:]+):[^@]+@#\1:****@#'))"
    if   probe copy; then AUDIO_MODE="copy"
    elif probe mp3;  then AUDIO_MODE="mp3"
    else
        log "WARN: $HW probes both failed — starting anyway so Settings is reachable"
    fi
    log "probe ok; hwaccel=$HW audio=$AUDIO_MODE"
fi

# ---- 3. Exec linda_cam -----------------------------------------------------
# cwd is /data (set in Dockerfile WORKDIR). The binary's os.Executable()
# returns /opt/linda_cam/linda_cam — its baseDir check looks for a sibling
# 'web' dir, doesn't find one, and falls back to cwd. So config.json,
# pictures/, hls/, *.db, and models/ all resolve to /data.
cd "$DATA"
exec env \
    LINDA_FFMPEG="$FF" \
    LINDA_AUDIO_MODE="$AUDIO_MODE" \
    LINDA_HWACCEL="$HW" \
    LD_LIBRARY_PATH="$APP/lib:${LD_LIBRARY_PATH:-}" \
    "$APP/linda_cam" "$@"
