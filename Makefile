GO ?= $(HOME)/.local/go/bin/go
NPM ?= npm

.PHONY: all web build run clean deps-web tidy

all: web build

deps-web:
	cd web && $(NPM) install

web:
	cd web && $(NPM) run build

build:
	$(GO) build -trimpath -ldflags="-s -w" -o linda_cam ./cmd/linda_cam

run: build
	LD_LIBRARY_PATH=$(CURDIR)/lib ./linda_cam

tidy:
	$(GO) mod tidy

clean:
	rm -rf linda_cam web/dist web/node_modules

fetch-model:
	mkdir -p models
	@if [ -s models/yolov8n.onnx ] && [ $$(stat -c%s models/yolov8n.onnx) -gt 1000000 ]; then \
		echo "models/yolov8n.onnx already present."; \
	else \
		echo "Ultralytics does not publish a yolov8n.onnx release asset."; \
		echo "The bundled detector supports any YOLOv8 ONNX. For bird/cat/dog + deer/fox,"; \
		echo "use the Open Images V7 variant (~600 classes including Deer and Fox):"; \
		echo "  pip install ultralytics"; \
		echo "  yolo export model=yolov8n-oiv7.pt format=onnx"; \
		echo "  mv yolov8n-oiv7.onnx models/yolov8n.onnx"; \
		echo "(The COCO model 'yolov8n.pt' works too but omits deer/fox.)"; \
		exit 1; \
	fi

fetch-onnxruntime:
	mkdir -p lib
	cd /tmp && curl -L -o ort.tgz https://github.com/microsoft/onnxruntime/releases/download/v1.20.1/onnxruntime-linux-x64-1.20.1.tgz && \
	tar -xzf ort.tgz && \
	cp onnxruntime-linux-x64-1.20.1/lib/libonnxruntime.so* $(CURDIR)/lib/ && \
	rm -rf /tmp/onnxruntime-linux-x64-1.20.1 /tmp/ort.tgz

fetch-ffmpeg:
	mkdir -p bin
	cd /tmp && curl -L -o ffmpeg.tar.xz https://johnvansickle.com/ffmpeg/releases/ffmpeg-release-amd64-static.tar.xz && \
	tar -xf ffmpeg.tar.xz && \
	cp ffmpeg-*-amd64-static/ffmpeg $(CURDIR)/bin/ && \
	rm -rf ffmpeg-*-amd64-static ffmpeg.tar.xz
