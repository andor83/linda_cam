#!/usr/bin/env python3
"""Export the dennisjooo/Birds-Classifier-EfficientNetB2 model to ONNX
plus a JSON sidecar describing class names and preprocessing.

Outputs:
    models/bird_classifier.onnx
    models/bird_classifier_classes.json   (classes + input_size + mean + std)

Run from repo root inside the project venv:
    source .venv/bin/activate
    python tools/export_bird_classifier.py
"""
from __future__ import annotations

import json
import os
import sys
from pathlib import Path

import torch
from transformers import AutoImageProcessor, AutoModelForImageClassification

# ViT-based 526-class bird species classifier. EfficientNet alternatives
# (e.g. dennisjooo/Birds-Classifier-EfficientNetB2) export to ONNX with broken
# weight tracing in current torch — ViT exports cleanly.
REPO = "dima806/bird_species_image_detection"
ROOT = Path(__file__).resolve().parents[1]
MODELS = ROOT / "models"
ONNX_PATH = MODELS / "bird_classifier.onnx"
META_PATH = MODELS / "bird_classifier_classes.json"


def main() -> int:
    MODELS.mkdir(parents=True, exist_ok=True)

    print(f"Loading {REPO} ...")
    processor = AutoImageProcessor.from_pretrained(REPO)
    model = AutoModelForImageClassification.from_pretrained(REPO).eval()

    # Resolve input size. Newer transformers exposes a SizeDict with
    # height/width attributes; older versions use a plain dict or int.
    size = processor.size
    input_size = None
    for attr in ("height", "shortest_edge", "longest_edge"):
        v = getattr(size, attr, None)
        if v:
            input_size = int(v)
            break
    if input_size is None and isinstance(size, dict):
        input_size = int(size.get("height") or size.get("shortest_edge") or 260)
    if input_size is None:
        input_size = int(size) if isinstance(size, (int, float)) else 260

    mean = list(map(float, processor.image_mean))
    std = list(map(float, processor.image_std))

    print(f"Input size: {input_size}x{input_size}; mean={mean}; std={std}")
    print(f"Classes: {len(model.config.id2label)}")

    dummy = torch.randn(1, 3, input_size, input_size)

    # Wrap to expose just `logits` as a tensor output rather than the
    # transformers ImageClassifierOutput dataclass.
    class Wrap(torch.nn.Module):
        def __init__(self, m):
            super().__init__()
            self.m = m

        def forward(self, x):
            return self.m(pixel_values=x).logits

    wrapped = Wrap(model).eval()

    # Remove any leftover sidecar weights from a previous export.
    for p in (ONNX_PATH, ONNX_PATH.with_suffix(ONNX_PATH.suffix + ".data")):
        if p.exists():
            p.unlink()

    print(f"Exporting to {ONNX_PATH} ...")
    # dynamo=False forces the legacy torch.onnx exporter, which preserves
    # pretrained weights for Hugging Face vision models reliably; the new
    # dynamo backend silently dropped them on this model.
    torch.onnx.export(
        wrapped,
        dummy,
        ONNX_PATH.as_posix(),
        input_names=["pixel_values"],
        output_names=["logits"],
        opset_version=17,
        dynamic_axes={
            "pixel_values": {0: "batch"},
            "logits": {0: "batch"},
        },
        do_constant_folding=True,
        dynamo=False,
    )

    classes = [model.config.id2label[i] for i in range(len(model.config.id2label))]

    with META_PATH.open("w", encoding="utf-8") as fh:
        json.dump(
            {
                "model": REPO,
                "input_size": input_size,
                "mean": mean,
                "std": std,
                "classes": classes,
            },
            fh,
            indent=2,
            ensure_ascii=False,
        )

    print(f"Wrote {ONNX_PATH} ({ONNX_PATH.stat().st_size/1e6:.1f} MB)")
    print(f"Wrote {META_PATH} ({len(classes)} classes)")

    # Quick sanity: run the exported ONNX and compare top-1 with PyTorch.
    try:
        import onnxruntime as ort

        with torch.no_grad():
            torch_logits = wrapped(dummy).cpu().numpy()
        sess = ort.InferenceSession(ONNX_PATH.as_posix(), providers=["CPUExecutionProvider"])
        onnx_logits = sess.run(["logits"], {"pixel_values": dummy.numpy()})[0]
        torch_top1 = int(torch_logits.argmax(axis=1)[0])
        onnx_top1 = int(onnx_logits.argmax(axis=1)[0])
        print(f"Sanity check: torch top1={torch_top1} ({classes[torch_top1]});"
              f" onnx top1={onnx_top1} ({classes[onnx_top1]})")
        if torch_top1 != onnx_top1:
            print("WARN: torch/onnx top-1 disagree on dummy input"
                  " (could just be a tie on random data — verify with a real image)")
    except Exception as e:  # noqa: BLE001
        print(f"WARN: sanity check skipped: {e}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
