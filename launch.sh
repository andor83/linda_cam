#!/usr/bin/env bash
# Pick the best available ffmpeg encoder, validate it against the configured
# RTSP source, then exec the linda_cam server.
#
# Runs on Linux and macOS. Encoder preference is per-platform, best first:
#   Linux  : h264_nvenc (NVIDIA)      -> libx264 on CPU
#   macOS  : h264_videotoolbox (Apple) -> libx264 on CPU
# LINDA_HWACCEL forces one mode: cuda | videotoolbox | none.
#
# Order of operations:
#   1. First-run init: apply DB schemas, export YOLO models if missing.
#   2. Find an ffmpeg binary offering the preferred encoder; fall back to CPU.
#   3. Read rtsp_url from config.json (may not exist yet on a true first run).
#   4. If RTSP URL is set, probe: 3-second encode to a throwaway dir with
#      AAC copy; fall back to MP3 audio.
#   5. Exec linda_cam with LINDA_FFMPEG, LINDA_HWACCEL and LINDA_AUDIO_MODE.

set -euo pipefail

# Resolve this script's directory without GNU readlink -f (absent on macOS).
SCRIPT_SRC="$0"
while [[ -h "$SCRIPT_SRC" ]]; do
    _dir="$(cd -P "$(dirname "$SCRIPT_SRC")" && pwd)"
    SCRIPT_SRC="$(readlink "$SCRIPT_SRC")"
    [[ "$SCRIPT_SRC" != /* ]] && SCRIPT_SRC="$_dir/$SCRIPT_SRC"
done
cd "$(cd -P "$(dirname "$SCRIPT_SRC")" && pwd)"

OS="$(uname -s)"

log() { printf '[launch] %s\n' "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

# stat(1) is incompatible between GNU and BSD; wrap the one field we need.
file_size() {
    if [[ "$OS" == "Darwin" ]]; then
        stat -f%z "$1"
    else
        stat -c%s "$1"
    fi
}

# ---- 0. First-run init ------------------------------------------------------

VENV_PY="$PWD/.venv/bin/python"

init_db() {
    local db="$1" sql="$2"
    if [[ -s "$db" ]]; then
        return 0
    fi
    if [[ ! -r "$sql" ]]; then
        die "schema file missing: $sql"
    fi
    command -v sqlite3 >/dev/null 2>&1 || die "sqlite3 not installed; needed to initialize $db"
    log "init $db from $(basename "$sql")"
    sqlite3 "$db" < "$sql" || die "failed to initialize $db"
}

init_db "$PWD/log.db"       "$PWD/schema/log.sql"
init_db "$PWD/sightings.db" "$PWD/schema/sightings.sql"

ensure_yolov8n() {
    local out="$PWD/models/yolov8n.onnx"
    if [[ -s "$out" ]] && (( $(file_size "$out") > 1000000 )); then
        return 0
    fi
    log "models/yolov8n.onnx missing — exporting via ultralytics"
    [[ -x "$VENV_PY" ]] || die "no .venv python at $VENV_PY; create it and 'pip install ultralytics' before first run"
    local pt="$PWD/yolov8n-oiv7.pt"
    [[ -s "$pt" ]] || die "yolov8n-oiv7.pt not found at repo root; download it from https://github.com/ultralytics/assets/releases/ first"
    mkdir -p "$PWD/models"
    # ultralytics writes the .onnx next to the .pt; export then move into place.
    (
        cd "$PWD"
        "$VENV_PY" -c "from ultralytics import YOLO; YOLO('$pt').export(format='onnx', opset=12, imgsz=640)"
    ) || die "ultralytics export failed"
    [[ -s "$PWD/yolov8n-oiv7.onnx" ]] || die "ultralytics did not produce yolov8n-oiv7.onnx"
    mv "$PWD/yolov8n-oiv7.onnx" "$out"
    log "wrote $out ($(file_size "$out") bytes)"
}

ensure_bird_classifier() {
    local out="$PWD/models/bird_classifier.onnx"
    local meta="$PWD/models/bird_classifier_classes.json"
    if [[ -s "$out" ]] && [[ -s "$meta" ]]; then
        return 0
    fi
    log "models/bird_classifier.onnx missing — running tools/export_bird_classifier.py"
    [[ -x "$VENV_PY" ]] || die "no .venv python at $VENV_PY; cannot export bird classifier"
    [[ -r "$PWD/tools/export_bird_classifier.py" ]] || die "tools/export_bird_classifier.py not found"
    "$VENV_PY" "$PWD/tools/export_bird_classifier.py" || die "bird classifier export failed"
}

ensure_yolov8n
ensure_bird_classifier

# ---- 1. Pick ffmpeg and the best available encoder --------------------------
# (numbering preserved from earlier revisions; first-run init is step 0 above.)

ffmpeg_candidates() {
    [[ -x ./bin/ffmpeg ]] && printf '%s\n' "$PWD/bin/ffmpeg"
    # /opt/homebrew is Apple-silicon Homebrew; /usr/local is Intel Homebrew.
    local p
    for p in /usr/bin/ffmpeg /usr/local/bin/ffmpeg /opt/homebrew/bin/ffmpeg; do
        [[ -x "$p" ]] && printf '%s\n' "$p"
    done
    command -v ffmpeg 2>/dev/null || true
}

# find_ffmpeg <encoder>  — first ffmpeg listing <encoder>; any ffmpeg if empty.
find_ffmpeg() {
    local want="$1" c encoders
    while IFS= read -r c; do
        [[ -n "$c" && -x "$c" ]] || continue
        if [[ -z "$want" ]]; then
            printf '%s' "$c"; return 0
        fi
        # Buffer the output — grep -q would close the pipe early and give
        # ffmpeg SIGPIPE, which pipefail then reports as failure.
        encoders=$("$c" -hide_banner -encoders 2>/dev/null) || continue
        if [[ "$encoders" == *"$want"* ]]; then
            printf '%s' "$c"; return 0
        fi
    done < <(ffmpeg_candidates)
    return 1
}

if [[ "$OS" == "Darwin" ]]; then
    PREFERRED_ACCEL="videotoolbox"; PREFERRED_ENC="h264_videotoolbox"
else
    PREFERRED_ACCEL="cuda";         PREFERRED_ENC="h264_nvenc"
fi

HW="${LINDA_HWACCEL:-}"
if [[ "$HW" == "none" ]]; then
    FF="$(find_ffmpeg "")" || die "no ffmpeg found"
    ACCEL="none"
    log "ffmpeg=$FF (LINDA_HWACCEL=none — libx264 on CPU)"
elif [[ -n "$HW" ]]; then
    case "$HW" in
        cuda)         forced_enc="h264_nvenc" ;;
        videotoolbox) forced_enc="h264_videotoolbox" ;;
        *) die "unknown LINDA_HWACCEL='$HW' (expected cuda, videotoolbox or none)" ;;
    esac
    FF="$(find_ffmpeg "$forced_enc")" \
        || die "LINDA_HWACCEL=$HW but no ffmpeg providing $forced_enc was found"
    ACCEL="$HW"
    log "ffmpeg=$FF ($forced_enc, forced by LINDA_HWACCEL)"
elif FF="$(find_ffmpeg "$PREFERRED_ENC")"; then
    ACCEL="$PREFERRED_ACCEL"
    log "ffmpeg=$FF ($PREFERRED_ENC available)"
elif FF="$(find_ffmpeg "")"; then
    ACCEL="none"
    log "ffmpeg=$FF (no $PREFERRED_ENC — falling back to libx264 on CPU)"
else
    die "no ffmpeg found (Linux: apt install ffmpeg / macOS: brew install ffmpeg)"
fi

# ---- 2. Read RTSP URL -------------------------------------------------------
# On a true first run config.json doesn't exist yet — the binary creates it
# and the user sets rtsp_url via the Settings UI. In that case skip the probe
# and start the binary with no audio mode set; the second launch (after the
# user enters a URL) will probe normally.

RTSP=""
if [[ -r ./config.json ]]; then
    RTSP="$(jq -r '.rtsp_url // empty' ./config.json)"
fi

# ---- 3/4. Probe AAC copy, then MP3 -----------------------------------------

PROBE_DIR="$(mktemp -d "${TMPDIR:-/tmp}/linda-probe-XXXXXX")"
trap 'rm -rf "$PROBE_DIR"' EXIT

probe() {
    local audio="$1" rc=0 stderr_log="$PROBE_DIR/ffmpeg.err"
    local -a args=(-hide_banner -loglevel error)
    case "$ACCEL" in
        cuda)         args+=(-hwaccel cuda -hwaccel_output_format cuda) ;;
        videotoolbox) args+=(-hwaccel videotoolbox) ;;
    esac
    args+=(-rtsp_transport tcp -i "$RTSP" -t 3)
    # Keep these in step with internal/stream/streamer.go's encoder ladder.
    case "$ACCEL" in
        cuda)
            args+=(-c:v h264_nvenc -preset p4 -rc vbr -cq 23 -b:v 5M -maxrate 8M -g 30) ;;
        videotoolbox)
            args+=(-c:v h264_videotoolbox -profile:v high -b:v 5M -maxrate 8M
                   -bufsize 10M -realtime 1 -g 30 -pix_fmt yuv420p) ;;
        *)
            args+=(-c:v libx264 -preset veryfast -tune zerolatency -crf 23
                   -maxrate 5M -bufsize 10M -g 30 -pix_fmt yuv420p) ;;
    esac
    if [[ "$audio" == "copy" ]]; then
        args+=(-c:a copy)
    else
        args+=(-c:a libmp3lame -b:a 128k)
    fi
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
        log "probe audio=$audio failed (rc=$rc):"
        sed 's/^/[ffmpeg] /' "$stderr_log" >&2 || true
        return $rc
    fi
    # Require that at least one segment was actually produced.
    if ! compgen -G "$PROBE_DIR/seg_*.m4s" >/dev/null; then
        log "probe audio=$audio produced no segments"
        sed 's/^/[ffmpeg] /' "$stderr_log" >&2 || true
        return 1
    fi
    return 0
}

if [[ -z "$RTSP" ]]; then
    log "no rtsp_url configured yet — skipping probe; set it in Settings, then restart"
    AUDIO_MODE="copy"
else
    log "rtsp_url resolved ($(printf '%s' "$RTSP" | sed -E 's#(://[^:]+):[^@]+@#\1:****@#'))"
    AUDIO_MODE=""
    if probe copy; then
        AUDIO_MODE="copy"
    elif probe mp3; then
        AUDIO_MODE="mp3"
    else
        die "$ACCEL probes with both AAC-copy and MP3 audio failed; stream will not start"
    fi
    log "probe ok; audio=$AUDIO_MODE"
fi

# ---- 5. Exec linda_cam ------------------------------------------------------

if [[ ! -x ./linda_cam ]]; then
    die "./linda_cam binary not found or not executable (run 'make build')"
fi

# macOS resolves the ONNX Runtime dylib by the absolute path main.go hands to
# onnxruntime_go, so DYLD_LIBRARY_PATH is only a fallback — and System
# Integrity Protection strips DYLD_* from protected processes anyway.
if [[ "$OS" == "Darwin" ]]; then
    exec env \
        LINDA_FFMPEG="$FF" \
        LINDA_HWACCEL="$ACCEL" \
        LINDA_AUDIO_MODE="$AUDIO_MODE" \
        DYLD_LIBRARY_PATH="$PWD/lib:${DYLD_LIBRARY_PATH:-}" \
        ./linda_cam
fi

# Extra library paths for ONNX Runtime CUDA EP. The pip wheels for ORT-GPU
# and its CUDA 12 runtime live under ~/.local/.../nvidia/*/lib; we only
# actually need these if the user has dropped a GPU-enabled libonnxruntime.so
# in ./lib, but including them unconditionally is harmless when absent.
CUDA_LIB_DIRS=""
for NV in "$HOME"/.local/lib/python3.*/site-packages/nvidia; do
    [[ -d "$NV" ]] || continue
    for d in "$NV/cuda_runtime/lib" "$NV/cublas/lib" "$NV/cufft/lib" \
             "$NV/curand/lib" "$NV/cudnn/lib" "$NV/nvjitlink/lib"; do
        [[ -d "$d" ]] && CUDA_LIB_DIRS+="$d:"
    done
done

exec env \
    LINDA_FFMPEG="$FF" \
    LINDA_HWACCEL="$ACCEL" \
    LINDA_AUDIO_MODE="$AUDIO_MODE" \
    LD_LIBRARY_PATH="$PWD/lib:${CUDA_LIB_DIRS}${LD_LIBRARY_PATH:-}" \
    ./linda_cam
