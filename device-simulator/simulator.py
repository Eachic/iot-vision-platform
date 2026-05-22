import argparse
import itertools
import os
import random
import time
from datetime import datetime, timezone
from pathlib import Path

import requests
from PIL import Image, ImageDraw


DEFAULT_TARGETS = {
    "edge": "http://127.0.0.1:8081/api/edge/upload",
    "gateway": "http://127.0.0.1:5173/api/images/upload",
}


def ensure_samples(image_dir: Path) -> None:
    image_dir.mkdir(parents=True, exist_ok=True)
    if any(image_dir.glob("*.png")) or any(image_dir.glob("*.jpg")) or any(image_dir.glob("*.jpeg")):
        return
    samples = [
        ("campus_gate", (78, 151, 116), "Campus Gate"),
        ("factory_device", (77, 115, 181), "Factory Device"),
        ("road_vehicle", (189, 94, 81), "Road Vehicle"),
        ("lab_scene", (142, 119, 184), "Lab Scene"),
    ]
    for name, color, label in samples:
        img = Image.new("RGB", (900, 560), color)
        draw = ImageDraw.Draw(img)
        for x in range(0, 900, 45):
            draw.line((x, 0, x + 180, 560), fill=tuple(min(255, c + 35) for c in color), width=3)
        draw.rectangle((60, 70, 840, 490), outline=(255, 255, 255), width=6)
        draw.text((90, 100), label, fill=(255, 255, 255))
        img.save(image_dir / f"{name}.png")


def iter_images(image_dir: Path):
    files = sorted([*image_dir.glob("*.png"), *image_dir.glob("*.jpg"), *image_dir.glob("*.jpeg")])
    if not files:
        raise RuntimeError(f"No images found in {image_dir}")
    while True:
        shuffled = files[:]
        random.shuffle(shuffled)
        yield from shuffled


def upload(upload_url: str, token: str, device_id: str, edge_node_id: str, path: Path) -> None:
    with path.open("rb") as fh:
        files = {"file": (path.name, fh, "application/octet-stream")}
        data = {
            "device_id": device_id,
            "edge_node_id": edge_node_id,
            "captured_at": datetime.now(timezone.utc).isoformat(),
        }
        resp = requests.post(
            upload_url,
            headers={"X-Device-Token": token},
            files=files,
            data=data,
            timeout=20,
        )
    print(f"[{device_id}] {path.name} -> {resp.status_code} {resp.text}")
    resp.raise_for_status()


def main():
    parser = argparse.ArgumentParser(description="Simulate IoT camera devices uploading images to the platform.")
    parser.add_argument("--device-id", default=os.getenv("DEVICE_ID", "device_001"))
    parser.add_argument("--edge-node-id", default=os.getenv("EDGE_NODE_ID", "edge_001"))
    parser.add_argument("--route", choices=sorted(DEFAULT_TARGETS), default=os.getenv("SIMULATOR_ROUTE", "edge"))
    parser.add_argument("--edge-url", default=os.getenv("EDGE_UPLOAD_URL", DEFAULT_TARGETS["edge"]))
    parser.add_argument("--gateway-url", default=os.getenv("GATEWAY_UPLOAD_URL", DEFAULT_TARGETS["gateway"]))
    parser.add_argument("--upload-url", default=os.getenv("UPLOAD_URL", ""))
    parser.add_argument("--interval", type=float, default=float(os.getenv("UPLOAD_INTERVAL_SECONDS", "3.0")))
    parser.add_argument("--image-dir", default=os.getenv("IMAGE_DIR", "./VOC2028/VOC2028/JPEGImages"))
    parser.add_argument("--token", default=os.getenv("DEVICE_TOKEN", "course-demo-token"))
    parser.add_argument("--once", action="store_true")
    args = parser.parse_args()

    targets = {
        "edge": args.edge_url,
        "gateway": args.gateway_url,
    }
    upload_url = args.upload_url or targets[args.route]
    print(f"[simulator] route={args.route} upload_url={upload_url}")

    image_dir = Path(args.image_dir)
    ensure_samples(image_dir)
    images = iter_images(image_dir)
    for path in itertools.islice(images, 1 if args.once else None):
        try:
            upload(upload_url, args.token, args.device_id, args.edge_node_id, path)
        except Exception as exc:
            print(f"[{args.device_id}] upload failed: {exc}")
        if args.once:
            break
        time.sleep(args.interval)


if __name__ == "__main__":
    main()
