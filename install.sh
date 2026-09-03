#!/usr/bin/env bash
# Interactive bare-metal installer for linda_cam.
#
# Installs linda_cam *in place* (the repo directory is the runtime directory —
# config.json, pictures/, models/ and the SQLite DBs all live next to the
# binary) and registers it with systemd so it starts at boot.
#
# Every step asks first. Answer 'n' to skip anything you've already done or
# want to handle yourself; nothing is installed without a 'y'.
#
#   ./install.sh              # interactive
#   ./install.sh --yes        # accept every step (unattended)
#   ./install.sh --user       # install a --user unit instead of a system one
#   ./install.sh --no-service # build/provision only, skip systemd
#   ./install.sh --help

set -euo pipefail

# ---- Constants --------------------------------------------------------------

APP_DIR="$(cd "$(dirname "$(readlink -f "$0")")" && pwd)"
SERVICE_NAME="linda_cam"
GO_VERSION="1.25.0"                 # matches go.mod / Dockerfile
ORT_VERSION="1.20.1"                # matches Makefile fetch-onnxruntime
NODE_MIN="20"
YOLO_WEIGHTS_URL="https://github.com/ultralytics/assets/releases/download/v8.3.0/yolov8n-oiv7.pt"

ASSUME_YES=0
SERVICE_SCOPE="system"
INSTALL_SERVICE=1
STEP=0

# ---- Output helpers ---------------------------------------------------------

if [[ -t 1 ]]; then
    B=$'\033[1m'; R=$'\033[0;31m'; G=$'\033[0;32m'; Y=$'\033[0;33m'; C=$'\033[0;36m'; N=$'\033[0m'
else
    B=""; R=""; G=""; Y=""; C=""; N=""
fi

step() { STEP=$((STEP + 1)); printf '\n%s=== Step %d: %s ===%s\n' "$B" "$STEP" "$*" "$N"; }
info() { printf '  %s\n' "$*"; }
ok()   { printf '  %s✓%s %s\n' "$G" "$N" "$*"; }
skip() { printf '  %s–%s %s\n' "$C" "$N" "$*"; }
warn() { printf '  %s!%s %s\n' "$Y" "$N" "$*" >&2; }
die()  { printf '\n%sERROR:%s %s\n' "$R" "$N" "$*" >&2; exit 1; }

usage() {
    # Print the header comment block (everything after the shebang up to the
    # first non-comment line), with the leading '# ' stripped.
    sed -n '2,${/^#/!q; s/^#\{1,\} \{0,1\}//; p;}' "$0"
    exit 0
}

# Ask a yes/no question on the terminal (not stdin, so `curl | bash` still works).
confirm() {
    local prompt="$1" default="${2:-y}" hint reply
    if (( ASSUME_YES )); then
        printf '  %s %s[auto-yes]%s\n' "$prompt" "$C" "$N"
        return 0
    fi
    [[ "$default" == "n" ]] && hint="[y/N]" || hint="[Y/n]"
    while true; do
        printf '  %s%s%s %s ' "$B" "$prompt" "$N" "$hint" >/dev/tty
        read -r reply </dev/tty || reply=""
        reply="${reply:-$default}"
        case "${reply,,}" in
            y|yes) return 0 ;;
            n|no)  return 1 ;;
            *)     printf '    please answer y or n\n' >/dev/tty ;;
        esac
    done
}

# Prompt for a value with a default.
ask() {
    local prompt="$1" default="$2" reply
    if (( ASSUME_YES )); then printf '%s' "$default"; return 0; fi
    printf '  %s%s%s [%s] ' "$B" "$prompt" "$N" "$default" >/dev/tty
    read -r reply </dev/tty || reply=""
    printf '%s' "${reply:-$default}"
}

# ---- Small utilities --------------------------------------------------------

have() { command -v "$1" >/dev/null 2>&1; }

# ver_ge A B  -> true when version A >= version B
ver_ge() { [[ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n1)" == "$2" ]]; }

arch_tag() {
    case "$(uname -m)" in
        x86_64|amd64)  printf 'amd64' ;;
        aarch64|arm64) printf 'arm64' ;;
        *) die "unsupported CPU architecture: $(uname -m)" ;;
    esac
}

run_as_root() {
    if [[ $EUID -eq 0 ]]; then
        "$@"
    else
        have sudo || die "need root for: $* (install sudo, or re-run this script as root)"
        sudo "$@"
    fi
}

# ---- Argument parsing -------------------------------------------------------

while [[ $# -gt 0 ]]; do
    case "$1" in
        -y|--yes)      ASSUME_YES=1 ;;
        --user)        SERVICE_SCOPE="user" ;;
        --system)      SERVICE_SCOPE="system" ;;
        --no-service)  INSTALL_SERVICE=0 ;;
        --name)        SERVICE_NAME="${2:?--name needs a value}"; shift ;;
        -h|--help)     usage ;;
        *)             die "unknown option: $1 (try --help)" ;;
    esac
    shift
done

if (( ! ASSUME_YES )) && [[ ! -r /dev/tty ]]; then
    die "no terminal available for prompts — re-run with --yes for an unattended install"
fi

[[ "$(uname -s)" == "Linux" ]] || die "this installer targets Linux (systemd); use Docker on other platforms"

# ---- Package manager --------------------------------------------------------

PM=""
for candidate in apt-get dnf yum pacman zypper; do
    if have "$candidate"; then PM="$candidate"; break; fi
done
PM_UPDATED=0

# pkg_name <generic-name> — translate a logical package to this distro's name.
pkg_name() {
    case "$1:$PM" in
        sqlite3:pacman|sqlite3:dnf|sqlite3:yum|sqlite3:zypper) printf 'sqlite' ;;
        pyvenv:apt-get) printf 'python3-venv' ;;
        pyvenv:pacman)  printf 'python' ;;
        pyvenv:*)       printf 'python3' ;;
        python3:pacman) printf 'python' ;;
        *)              printf '%s' "$1" ;;
    esac
}

pkg_install() {
    [[ -n "$PM" ]] || die "no supported package manager found; install these manually: $*"
    case "$PM" in
        apt-get)
            if (( ! PM_UPDATED )); then
                info "running apt-get update ..."
                run_as_root apt-get update -qq && PM_UPDATED=1
            fi
            run_as_root apt-get install -y "$@"
            ;;
        dnf|yum) run_as_root "$PM" install -y "$@" ;;
        pacman)  run_as_root pacman -Sy --needed --noconfirm "$@" ;;
        zypper)  run_as_root zypper --non-interactive install "$@" ;;
    esac
}

# ---- Banner -----------------------------------------------------------------

cat <<BANNER
${B}linda_cam installer${N}

  app directory : ${APP_DIR}
  distro        : $( . /etc/os-release 2>/dev/null && printf '%s' "${PRETTY_NAME:-unknown}" )
  package mgr   : ${PM:-none detected}
  service scope : ${SERVICE_SCOPE}$( (( INSTALL_SERVICE )) || printf ' (skipped)' )

linda_cam runs from this directory: the binary, models, captured pictures and
the SQLite databases all live here, so keep it on a disk with room to spare.
Each step below asks before doing anything.
BANNER

if ! confirm "Continue with the install?"; then
    info "aborted — nothing was changed."
    exit 0
fi

# ---- Step 1: system packages ------------------------------------------------

step "System dependencies"

declare -a WANT_PKGS=()
missing_note() { warn "$1 is missing"; }

have sqlite3 || { missing_note "sqlite3 (used to create log.db / sightings.db)"; WANT_PKGS+=("$(pkg_name sqlite3)"); }
have jq      || { missing_note "jq (launch.sh reads config.json with it)";       WANT_PKGS+=("$(pkg_name jq)"); }
have curl    || { missing_note "curl (downloads models and ONNX Runtime)";       WANT_PKGS+=("$(pkg_name curl)"); }
have tar     || { missing_note "tar";                                            WANT_PKGS+=("$(pkg_name tar)"); }
have ffmpeg  || { missing_note "ffmpeg (RTSP ingest + HLS encoding)";            WANT_PKGS+=("$(pkg_name ffmpeg)"); }
have python3 || { missing_note "python3 (first-run ONNX model export)";          WANT_PKGS+=("$(pkg_name python3)"); }
if have python3 && ! python3 -c 'import venv' >/dev/null 2>&1; then
    missing_note "python3 venv module (first-run ONNX model export)"
    WANT_PKGS+=("$(pkg_name pyvenv)")
fi

if (( ${#WANT_PKGS[@]} == 0 )); then
    ok "all system dependencies present"
else
    info "would install: ${WANT_PKGS[*]}"
    if confirm "Install the missing system packages with ${PM:-<none>}?"; then
        pkg_install "${WANT_PKGS[@]}"
        ok "system packages installed"
    else
        skip "package install declined — later steps may fail without them"
    fi
fi

# ---- Step 2: Go toolchain ---------------------------------------------------

step "Go toolchain (>= ${GO_VERSION})"

GO_BIN=""
for candidate in "$(command -v go 2>/dev/null || true)" "$HOME/.local/go/bin/go" /usr/local/go/bin/go; do
    [[ -n "$candidate" && -x "$candidate" ]] || continue
    found_ver="$("$candidate" env GOVERSION 2>/dev/null | sed 's/^go//')"
    if [[ -n "$found_ver" ]] && ver_ge "$found_ver" "$GO_VERSION"; then
        GO_BIN="$candidate"
        ok "found Go $found_ver at $candidate"
        break
    elif [[ -n "$found_ver" ]]; then
        warn "Go $found_ver at $candidate is older than $GO_VERSION"
    fi
done

if [[ -z "$GO_BIN" ]]; then
    GO_TGZ="go${GO_VERSION}.linux-$(arch_tag).tar.gz"
    info "no suitable Go found; would download https://go.dev/dl/${GO_TGZ}"
    info "and unpack it to $HOME/.local/go (no root needed)"
    if confirm "Install Go ${GO_VERSION} into \$HOME/.local/go?"; then
        tmp="$(mktemp -d)"
        trap 'rm -rf "$tmp"' EXIT
        curl -fL --progress-bar -o "$tmp/go.tgz" "https://go.dev/dl/${GO_TGZ}" \
            || die "failed to download Go"
        mkdir -p "$HOME/.local"
        rm -rf "$HOME/.local/go"
        tar -C "$HOME/.local" -xzf "$tmp/go.tgz" || die "failed to unpack Go"
        rm -rf "$tmp"; trap - EXIT
        GO_BIN="$HOME/.local/go/bin/go"
        [[ -x "$GO_BIN" ]] || die "Go install did not produce $GO_BIN"
        ok "installed $("$GO_BIN" env GOVERSION) to $HOME/.local/go"
        info "add it to your PATH with: export PATH=\$HOME/.local/go/bin:\$PATH"
    else
        skip "Go install declined — the build step will be skipped"
    fi
fi

# ---- Step 3: Node.js / npm --------------------------------------------------

step "Node.js ${NODE_MIN}+ and npm (builds the Vue UI)"

# nvm-managed installs aren't on a non-login PATH; pull one in if that's all there is.
if ! have node && [[ -s "$HOME/.nvm/nvm.sh" ]]; then
    info "sourcing ~/.nvm/nvm.sh to find a Node install"
    # shellcheck disable=SC1091
    . "$HOME/.nvm/nvm.sh" >/dev/null 2>&1 || true
fi

NODE_OK=0
if have node && have npm; then
    node_ver="$(node --version 2>/dev/null | sed 's/^v//')"
    if ver_ge "$node_ver" "$NODE_MIN"; then
        NODE_OK=1
        ok "found Node $node_ver ($(command -v node))"
    else
        warn "Node $node_ver is older than the required $NODE_MIN"
    fi
else
    warn "node/npm not found"
fi

if (( ! NODE_OK )); then
    if [[ -d "$APP_DIR/internal/web/dist" ]] && [[ -n "$(ls -A "$APP_DIR/internal/web/dist" 2>/dev/null)" ]]; then
        info "a prebuilt UI already exists in internal/web/dist — Node is only needed to rebuild it"
    fi
    if confirm "Install Node.js from your distro's packages?" n; then
        pkg_install "$(pkg_name nodejs)" "$(pkg_name npm)" || true
        if have node && have npm; then
            NODE_OK=1
            ok "Node $(node --version) installed"
        else
            warn "Node still not available after the package install"
        fi
    else
        skip "Node install declined — the UI build step will be skipped"
    fi
fi

# ---- Step 4: ONNX Runtime shared library ------------------------------------

step "ONNX Runtime ${ORT_VERSION} (lib/libonnxruntime.so)"

if compgen -G "$APP_DIR/lib/libonnxruntime.so*" >/dev/null; then
    ok "already present in $APP_DIR/lib"
elif confirm "Download ONNX Runtime ${ORT_VERSION} into $APP_DIR/lib?"; then
    case "$(arch_tag)" in
        amd64) ort_arch="x64" ;;
        arm64) ort_arch="aarch64" ;;
    esac
    ort_name="onnxruntime-linux-${ort_arch}-${ORT_VERSION}"
    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' EXIT
    curl -fL --progress-bar -o "$tmp/ort.tgz" \
        "https://github.com/microsoft/onnxruntime/releases/download/v${ORT_VERSION}/${ort_name}.tgz" \
        || die "failed to download ONNX Runtime"
    tar -C "$tmp" -xzf "$tmp/ort.tgz" || die "failed to unpack ONNX Runtime"
    mkdir -p "$APP_DIR/lib"
    cp -a "$tmp/$ort_name/lib/"libonnxruntime.so* "$APP_DIR/lib/" || die "failed to copy libonnxruntime.so"
    rm -rf "$tmp"; trap - EXIT
    ok "installed $(ls "$APP_DIR/lib" | tr '\n' ' ')"
else
    skip "ONNX Runtime skipped — detection and classification will not start"
fi

# ---- Step 5: Python venv for model export -----------------------------------

step "Python venv for ONNX model export"

MODELS_PRESENT=0
if [[ -s "$APP_DIR/models/yolov8n.onnx" && -s "$APP_DIR/models/bird_classifier.onnx" \
      && -s "$APP_DIR/models/bird_classifier_classes.json" ]]; then
    MODELS_PRESENT=1
fi

if (( MODELS_PRESENT )); then
    ok "models/ already holds the detector and bird classifier — no export needed"
elif [[ -x "$APP_DIR/.venv/bin/python" ]] \
     && "$APP_DIR/.venv/bin/python" -c 'import ultralytics, transformers, torch' >/dev/null 2>&1; then
    ok ".venv already has torch + transformers + ultralytics"
else
    info "models are missing, so launch.sh will export them on first start."
    info "That needs a .venv here with torch, transformers and ultralytics"
    info "(a CPU-only torch wheel — roughly 2-3 GB of downloads)."
    if confirm "Create $APP_DIR/.venv and install the export dependencies?"; then
        have python3 || die "python3 is required but not installed"
        [[ -x "$APP_DIR/.venv/bin/python" ]] || python3 -m venv "$APP_DIR/.venv" \
            || die "failed to create the venv (is python3-venv installed?)"
        VPIP="$APP_DIR/.venv/bin/pip"
        "$VPIP" install --upgrade pip >/dev/null || warn "pip self-upgrade failed; continuing"
        # Pins mirror the Dockerfile model-builder stage.
        "$VPIP" install "torch==2.7.*" "torchvision==0.22.*" \
            --index-url https://download.pytorch.org/whl/cpu || die "torch install failed"
        "$VPIP" install "ultralytics==8.3.*" "transformers==4.46.*" \
            "onnx==1.18.*" "onnxruntime==1.22.*" || die "model export deps install failed"
        # ultralytics pulls in full opencv; the headless build avoids needing X libs.
        "$VPIP" uninstall -y opencv-python >/dev/null 2>&1 || true
        "$VPIP" install opencv-python-headless || warn "opencv-python-headless install failed"
        ok "venv ready at $APP_DIR/.venv"
    else
        skip "venv skipped — drop your own ONNX models into $APP_DIR/models/ instead"
    fi
fi

# ---- Step 6: YOLOv8 weights -------------------------------------------------

step "YOLOv8 Open Images V7 weights"

if (( MODELS_PRESENT )) || [[ -s "$APP_DIR/models/yolov8n.onnx" ]]; then
    ok "models/yolov8n.onnx already exported"
elif [[ -s "$APP_DIR/yolov8n-oiv7.pt" ]]; then
    ok "yolov8n-oiv7.pt already downloaded (launch.sh will export it on first start)"
elif confirm "Download yolov8n-oiv7.pt (~13 MB) so first start can export the detector?"; then
    curl -fL --progress-bar -o "$APP_DIR/yolov8n-oiv7.pt" "$YOLO_WEIGHTS_URL" \
        || die "failed to download the YOLOv8 weights"
    ok "saved $APP_DIR/yolov8n-oiv7.pt"
else
    skip "weights skipped — first start will fail unless models/yolov8n.onnx exists"
fi

# ---- Step 7: build the web UI -----------------------------------------------

step "Build the Vue UI"

if (( ! NODE_OK )); then
    skip "no usable Node — skipping (internal/web/dist must already be populated)"
elif confirm "Run 'npm install' and 'npm run build' in web/?"; then
    ( cd "$APP_DIR/web" && npm install ) || die "npm install failed"
    ( cd "$APP_DIR/web" && npm run build ) || die "npm run build failed"
    ok "UI built into internal/web/dist"
else
    skip "UI build skipped"
fi

# ---- Step 8: build the binary -----------------------------------------------

step "Build the linda_cam binary"

if [[ -z "$GO_BIN" ]]; then
    skip "no usable Go toolchain — skipping build"
elif confirm "Compile ./linda_cam with $GO_BIN?"; then
    ( cd "$APP_DIR" && "$GO_BIN" build -trimpath -ldflags="-s -w" -o linda_cam ./cmd/linda_cam ) \
        || die "go build failed"
    ok "built $APP_DIR/linda_cam"
else
    skip "build skipped"
fi

[[ -x "$APP_DIR/linda_cam" ]] || warn "no linda_cam binary yet — the service will not start until one is built"

# ---- Step 9: runtime directories and databases ------------------------------

step "Runtime directories and SQLite databases"

if confirm "Create pictures/, hls/, models/ and initialize log.db + sightings.db?"; then
    mkdir -p "$APP_DIR/pictures" "$APP_DIR/hls" "$APP_DIR/models" "$APP_DIR/bin" "$APP_DIR/lib"
    if have sqlite3; then
        for pair in "log.db:schema/log.sql" "sightings.db:schema/sightings.sql"; do
            db="$APP_DIR/${pair%%:*}"; sql="$APP_DIR/${pair##*:}"
            if [[ -s "$db" ]]; then
                skip "$(basename "$db") already exists — left untouched"
            elif [[ -r "$sql" ]]; then
                sqlite3 "$db" < "$sql" || die "failed to initialize $db"
                ok "created $(basename "$db")"
            else
                warn "missing schema file $sql"
            fi
        done
    else
        warn "sqlite3 not installed — launch.sh will try to create the databases at startup"
    fi
    ok "runtime directories ready"
else
    skip "directory/database setup skipped"
fi

# ---- Step 10: NVENC check ---------------------------------------------------

step "Hardware encoder check"

NVENC_OK=0
if have ffmpeg; then
    # Buffer the encoder list: `| grep -q` would close the pipe early and give
    # ffmpeg SIGPIPE, which pipefail then reports as a failure.
    if encoders="$(ffmpeg -hide_banner -encoders 2>/dev/null)" && [[ "$encoders" == *h264_nvenc* ]]; then
        NVENC_OK=1
    fi
fi

if (( NVENC_OK )); then
    ok "ffmpeg supports h264_nvenc"
else
    warn "no ffmpeg with h264_nvenc found."
    warn "launch.sh requires NVENC and will exit at startup without it."
    warn "Options: install an NVENC-capable ffmpeg + NVIDIA drivers, or run the"
    warn "CPU-only Docker image instead (docker compose up -d)."
    if ! confirm "Continue anyway?"; then
        die "aborted at the encoder check"
    fi
fi

# ---- Step 11: systemd unit --------------------------------------------------

if (( ! INSTALL_SERVICE )); then
    step "systemd service"
    skip "--no-service given — skipping service installation"
else

step "Install the systemd service"

have systemctl || die "systemd not found; re-run with --no-service and start ./launch.sh yourself"

if [[ "$SERVICE_SCOPE" == "system" ]]; then
    default_user="$(stat -c '%U' "$APP_DIR")"
    while true; do
        RUN_USER="$(ask "Run the service as which user?" "$default_user")"
        if id "$RUN_USER" >/dev/null 2>&1; then break; fi
        warn "user '$RUN_USER' does not exist"
        (( ASSUME_YES )) && die "cannot resolve a service user"
    done
    RUN_GROUP="$(id -gn "$RUN_USER")"
    UNIT_PATH="/etc/systemd/system/${SERVICE_NAME}.service"
    SYSTEMCTL=(run_as_root systemctl)
    IDENTITY=$'User='"$RUN_USER"$'\nGroup='"$RUN_GROUP"$'\n'
    WANTED_BY="multi-user.target"
else
    RUN_USER="$(id -un)"
    UNIT_PATH="$HOME/.config/systemd/user/${SERVICE_NAME}.service"
    SYSTEMCTL=(systemctl --user)
    IDENTITY=""
    WANTED_BY="default.target"
fi

UNIT_TEXT="[Unit]
Description=Linda Cam — RTSP viewer with HLS streaming and YOLO detection
Documentation=https://github.com/linda/linda_cam
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
${IDENTITY}WorkingDirectory=${APP_DIR}
Environment=LD_LIBRARY_PATH=${APP_DIR}/lib
ExecStart=${APP_DIR}/launch.sh
Restart=on-failure
RestartSec=5s

NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=${APP_DIR}
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true

[Install]
WantedBy=${WANTED_BY}
"

info "unit file: $UNIT_PATH"
printf '%s\n' "$UNIT_TEXT" | sed 's/^/    | /'

WRITE_UNIT=1
if [[ -e "$UNIT_PATH" ]]; then
    confirm "$UNIT_PATH already exists — overwrite it?" || WRITE_UNIT=0
else
    confirm "Write this unit file?" || WRITE_UNIT=0
fi

if (( ! WRITE_UNIT )); then
    skip "unit file not written"
else
    chmod +x "$APP_DIR/launch.sh"
    if [[ "$SERVICE_SCOPE" == "system" ]]; then
        printf '%s' "$UNIT_TEXT" | run_as_root tee "$UNIT_PATH" >/dev/null
    else
        mkdir -p "$(dirname "$UNIT_PATH")"
        printf '%s' "$UNIT_TEXT" > "$UNIT_PATH"
    fi
    "${SYSTEMCTL[@]}" daemon-reload
    ok "wrote $UNIT_PATH"
fi

# ---- Step 12: enable and start ----------------------------------------------

step "Enable and start ${SERVICE_NAME}"

if [[ ! -e "$UNIT_PATH" ]]; then
    skip "no unit file installed — nothing to enable"
else
    if confirm "Enable ${SERVICE_NAME} to start at boot?"; then
        if "${SYSTEMCTL[@]}" enable "${SERVICE_NAME}.service"; then
            ok "enabled"
        else
            warn "systemctl enable failed"
        fi
        if [[ "$SERVICE_SCOPE" == "user" ]]; then
            info "user services only run while you're logged in unless lingering is on"
            if confirm "Enable lingering so it starts at boot without a login?"; then
                run_as_root loginctl enable-linger "$RUN_USER" && ok "lingering enabled for $RUN_USER" || \
                    warn "loginctl enable-linger failed"
            fi
        fi
    else
        skip "not enabled at boot"
    fi

    if confirm "Start ${SERVICE_NAME} now?"; then
        if "${SYSTEMCTL[@]}" restart "${SERVICE_NAME}.service"; then
            ok "started"
        else
            warn "start failed — check the logs below"
        fi
        "${SYSTEMCTL[@]}" --no-pager --full status "${SERVICE_NAME}.service" || true
    else
        skip "not started"
    fi
fi

fi  # INSTALL_SERVICE

# ---- Summary ----------------------------------------------------------------

if [[ "$SERVICE_SCOPE" == "system" ]]; then
    CTL="sudo systemctl"; JOURNAL="sudo journalctl -u ${SERVICE_NAME} -f"
else
    CTL="systemctl --user"; JOURNAL="journalctl --user -u ${SERVICE_NAME} -f"
fi

PORT="8001"
if [[ -r "$APP_DIR/config.json" ]] && have jq; then
    addr="$(jq -r '.http_addr // empty' "$APP_DIR/config.json")"
    if [[ "$addr" == *:* ]]; then PORT="${addr##*:}"; fi
fi

cat <<SUMMARY

${B}Done.${N}

  Open           http://localhost:${PORT}
                 First visit asks you to set a password; then paste your
                 camera's RTSP URL into Settings, Save, and restart the
                 service so launch.sh can probe the stream.

  Status         ${CTL} status ${SERVICE_NAME}
  Logs           ${JOURNAL}
  Restart        ${CTL} restart ${SERVICE_NAME}
  Stop / disable ${CTL} stop ${SERVICE_NAME} && ${CTL} disable ${SERVICE_NAME}

  Runtime files  ${APP_DIR}/{config.json,log.db,sightings.db,pictures/,models/}
SUMMARY
