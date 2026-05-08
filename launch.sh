#!/usr/bin/env bash
# Validate an NVENC-capable ffmpeg against the configured RTSP source, then
# exec the linda_cam server with the resolved ffmpeg path and audio mode.
#
# Order of operations:
#   1. First-run init: apply DB schemas, export YOLO models if missing.
#   2. Find an ffmpeg binary that lists h264_nvenc.
#   3. Read rtsp_url from config.json (may not exist yet on a true first run).
#   4. If RTSP URL is set, probe: 3-second NVENC + AAC-copy encode to a
#      throwaway dir; fall back to MP3 audio.
#   5. Exec linda_cam with LINDA_FFMPEG and LINDA_AUDIO_MODE set.

set -euo pipefail

cd "$(dirname "$(readlink -f "$0")")"

log() { printf '[launch] %s\n' "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

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
    if [[ -s "$out" ]] && (( $(stat -c%s "$out") > 1000000 )); then
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
    log "wrote $out ($(stat -c%s "$out") bytes)"
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

# ---- 1. Pick ffmpeg ---------------------------------------------------------
# (numbering preserved from earlier revisions; first-run init is step 0 above.)

pick_ffmpeg() {
    local candidates=()
    [[ -x ./bin/ffmpeg ]]      && candidates+=("$PWD/bin/ffmpeg")
    [[ -x /usr/bin/ffmpeg ]]   && candidates+=(/usr/bin/ffmpeg)
    [[ -x /usr/local/bin/ffmpeg ]] && candidates+=(/usr/local/bin/ffmpeg)
    if command -v ffmpeg >/dev/null 2>&1; then
        candidates+=("$(command -v ffmpeg)")
    fi
    for c in "${candidates[@]}"; do
        # Buffer the output — grep -q would close the pipe early and give
        # ffmpeg SIGPIPE, which pipefail then reports as failure.
        local encoders
        encoders=$("$c" -hide_banner -encoders 2>/dev/null) || continue
        if [[ "$encoders" == *h264_nvenc* ]]; then
            printf '%s' "$c"
            return 0
        fi
    done
    return 1
}

FF="$(pick_ffmpeg)" || die "no ffmpeg with h264_nvenc found (install system ffmpeg built against nvenc; bundled static build does not have it)"
log "ffmpeg=$FF (h264_nvenc available)"

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

PROBE_DIR="$(mktemp -d -t linda-probe-XXXXXX)"
trap 'rm -rf "$PROBE_DIR"' EXIT

probe() {
    local audio="$1" rc=0 stderr_log="$PROBE_DIR/ffmpeg.err"
    local -a args=(
        -hide_banner -loglevel error
        -hwaccel cuda -hwaccel_output_format cuda
        -rtsp_transport tcp
        -i "$RTSP"
        -t 3
        -c:v h264_nvenc -preset p4 -rc vbr -cq 23 -b:v 5M -maxrate 8M -g 30
    )
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
        die "NVENC+AAC and NVENC+MP3 probes both failed; stream will not start"
    fi
    log "probe ok; audio=$AUDIO_MODE"
fi

# ---- 5. Exec linda_cam ------------------------------------------------------

if [[ ! -x ./linda_cam ]]; then
    die "./linda_cam binary not found or not executable (run 'make build')"
fi

# Extra library paths for ONNX Runtime CUDA EP. The pip wheels for ORT-GPU
# and its CUDA 12 runtime live under ~/.local/.../nvidia/*/lib; we only
# actually need these if the user has dropped a GPU-enabled libonnxruntime.so
# in ./lib, but including them unconditionally is harmless when absent.
NV=/home/andy/.local/lib/python3.13/site-packages/nvidia
CUDA_LIB_DIRS=""
for d in "$NV/cuda_runtime/lib" "$NV/cublas/lib" "$NV/cufft/lib" \
         "$NV/curand/lib" "$NV/cudnn/lib" "$NV/nvjitlink/lib"; do
    [[ -d "$d" ]] && CUDA_LIB_DIRS+="$d:"
done

exec env \
    LINDA_FFMPEG="$FF" \
    LINDA_AUDIO_MODE="$AUDIO_MODE" \
    LD_LIBRARY_PATH="$PWD/lib:${CUDA_LIB_DIRS}${LD_LIBRARY_PATH:-}" \
    ./linda_cam
