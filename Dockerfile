# syntax=docker/dockerfile:1.7

# ---- 1. Web SPA -------------------------------------------------------------
FROM node:22-bookworm AS web-builder
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build
# Build output lands at /src/internal/web/dist (per vite.config.ts outDir).

# ---- 2. Model exporter ------------------------------------------------------
# Exports yolov8n.onnx (Open Images V7) and bird_classifier.onnx using the
# same Python pipeline as launch.sh's first-run logic. Output goes to
# /out/models/ which the runtime image bakes in as a default seed for the
# data volume.
FROM ubuntu:25.10 AS model-builder
ENV DEBIAN_FRONTEND=noninteractive PIP_DISABLE_PIP_VERSION_CHECK=1
RUN apt-get update && apt-get install -y --no-install-recommends \
        python3 python3-venv python3-pip ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /work
RUN python3 -m venv /opt/venv
ENV PATH=/opt/venv/bin:$PATH
RUN pip install --no-cache-dir \
        "torch==2.7.*" \
        "torchvision==0.22.*" \
        --index-url https://download.pytorch.org/whl/cpu \
 && pip install --no-cache-dir \
        "ultralytics==8.3.*" \
        "transformers==4.46.*" \
        "onnx==1.18.*" \
        "onnxruntime==1.22.*" \
 && pip uninstall -y opencv-python \
 && pip install --no-cache-dir opencv-python-headless

# Pull the YOLOv8 OIV7 weights and export to ONNX.
RUN mkdir -p /out/models && \
    curl -fL -o /work/yolov8n-oiv7.pt \
        https://github.com/ultralytics/assets/releases/download/v8.3.0/yolov8n-oiv7.pt && \
    cd /work && \
    python -c "from ultralytics import YOLO; YOLO('yolov8n-oiv7.pt').export(format='onnx', opset=12, imgsz=640)" && \
    mv /work/yolov8n-oiv7.onnx /out/models/yolov8n.onnx

# Bird classifier: dima806/bird_species_image_detection -> ONNX + classes JSON.
COPY tools/export_bird_classifier.py /work/tools/export_bird_classifier.py
RUN mkdir -p /work/models && \
    cd /work && \
    python tools/export_bird_classifier.py && \
    mv /work/models/bird_classifier.onnx /out/models/bird_classifier.onnx && \
    mv /work/models/bird_classifier_classes.json /out/models/bird_classifier_classes.json

# ---- 3. Go binary -----------------------------------------------------------
FROM ubuntu:25.10 AS go-builder
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl gcc libc6-dev pkg-config \
    && rm -rf /var/lib/apt/lists/*
ARG GO_VERSION=1.25.0
RUN curl -fL -o /tmp/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" && \
    tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz
ENV PATH=/usr/local/go/bin:$PATH GOCACHE=/root/.cache/go-build GOMODCACHE=/go/pkg/mod
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web-builder /src/internal/web/dist ./internal/web/dist
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/linda_cam ./cmd/linda_cam

# ---- 4. Runtime -------------------------------------------------------------
FROM ubuntu:25.10 AS runtime
ENV DEBIAN_FRONTEND=noninteractive
# System ffmpeg has h264_nvenc compiled in (works at runtime with NVIDIA Container
# Toolkit + GPU pass-through). jq + sqlite3 are used by the launch script.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl ffmpeg jq sqlite3 xz-utils tzdata \
    && rm -rf /var/lib/apt/lists/*

# ONNX Runtime shared library (matches Makefile fetch-onnxruntime).
ARG ORT_VERSION=1.20.1
RUN mkdir -p /opt/linda_cam/lib && \
    curl -fL -o /tmp/ort.tgz \
        "https://github.com/microsoft/onnxruntime/releases/download/v${ORT_VERSION}/onnxruntime-linux-x64-${ORT_VERSION}.tgz" && \
    tar -xzf /tmp/ort.tgz -C /tmp && \
    cp -a /tmp/onnxruntime-linux-x64-${ORT_VERSION}/lib/libonnxruntime.so* /opt/linda_cam/lib/ && \
    rm -rf /tmp/onnxruntime-linux-x64-${ORT_VERSION} /tmp/ort.tgz

# Static ffmpeg fallback (no NVENC, but useful for the JPEG-extraction path
# and as a safety net). launch.sh's pick_ffmpeg checks ./bin/ffmpeg first
# but only accepts h264_nvenc; the entrypoint exposes the static binary on
# PATH so the Go binary's JPEG ffmpeg lookup can use it on CPU-only hosts.
RUN mkdir -p /opt/linda_cam/bin && \
    curl -fL -o /tmp/ffmpeg.tar.xz \
        https://johnvansickle.com/ffmpeg/releases/ffmpeg-release-amd64-static.tar.xz && \
    tar -xf /tmp/ffmpeg.tar.xz -C /tmp && \
    cp /tmp/ffmpeg-*-amd64-static/ffmpeg /opt/linda_cam/bin/ffmpeg-static && \
    rm -rf /tmp/ffmpeg-*-amd64-static /tmp/ffmpeg.tar.xz

# App layout. Note: NO web/ subdirectory next to the binary — that makes the
# Go binary's baseDir-resolution fall back to cwd (= /data), so config.json,
# pictures/, models/, *.db all land on the bind-mounted volume.
COPY --from=go-builder /out/linda_cam        /opt/linda_cam/linda_cam
COPY --from=model-builder /out/models        /opt/linda_cam/models-default
COPY schema/                                  /opt/linda_cam/schema/
COPY docker/entrypoint.sh                     /opt/linda_cam/entrypoint.sh
RUN chmod +x /opt/linda_cam/entrypoint.sh /opt/linda_cam/linda_cam /opt/linda_cam/bin/ffmpeg-static

ENV LD_LIBRARY_PATH=/opt/linda_cam/lib \
    LINDA_APP_DIR=/opt/linda_cam \
    LINDA_DATA_DIR=/data

EXPOSE 8001
VOLUME ["/data"]
WORKDIR /data
ENTRYPOINT ["/opt/linda_cam/entrypoint.sh"]
