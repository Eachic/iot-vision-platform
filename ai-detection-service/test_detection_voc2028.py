import argparse
import statistics
import sys
import time
from pathlib import Path

import grpc
from PIL import Image, ImageDraw


REPO_ROOT = Path(__file__).resolve().parents[1]
AI_SERVICE_PROTO_ROOT = REPO_ROOT / "ai-service"
if str(AI_SERVICE_PROTO_ROOT) not in sys.path:
    sys.path.insert(0, str(AI_SERVICE_PROTO_ROOT))

from proto.vision.v1 import vision_pb2  # noqa: E402


METHOD = "/vision.v1.VisionAnalysisService/AnalyzeImage"


def main():
    parser = argparse.ArgumentParser(description="Smoke test local GPU detection service with VOC2028 images.")
    parser.add_argument("--addr", default="127.0.0.1:9100")
    parser.add_argument("--dataset", default=str(REPO_ROOT / "VOC2028" / "VOC2028" / "JPEGImages"))
    parser.add_argument("--output", default=str(Path(__file__).resolve().parent / "test-output"))
    parser.add_argument("--limit", type=int, default=10)
    args = parser.parse_args()

    image_dir = Path(args.dataset)
    output_dir = Path(args.output)
    output_dir.mkdir(parents=True, exist_ok=True)
    images = sorted([p for p in image_dir.iterdir() if p.suffix.lower() in {".jpg", ".jpeg", ".png"}])[: args.limit]
    if not images:
        raise SystemExit(f"no images found under {image_dir}")

    channel = grpc.insecure_channel(args.addr)
    analyze = channel.unary_unary(
        METHOD,
        request_serializer=lambda msg: msg.SerializeToString(),
        response_deserializer=parse_response,
    )

    latencies = []
    for image_path in images:
        req = vision_pb2.AnalyzeImageRequest(
            request_id=f"test_{image_path.stem}",
            image_id=image_path.stem,
            image_uri=str(image_path),
            filename=image_path.name,
            content_type="image/jpeg",
            tasks=[vision_pb2.AnalysisTask(type="detection")],
        )
        started = time.perf_counter()
        resp = analyze(req, timeout=60)
        elapsed = (time.perf_counter() - started) * 1000
        latencies.append(elapsed)
        detections = collect_detections(resp)
        output_path = output_dir / image_path.name
        draw_detections(image_path, output_path, detections)
        print(f"{image_path.name}: boxes={len(detections)} grpc_latency={elapsed:.1f}ms model_latency={resp.latency_ms}ms -> {output_path}")

    print(f"average grpc latency: {statistics.mean(latencies):.1f}ms over {len(latencies)} images")


def parse_response(data):
    resp = vision_pb2.AnalyzeImageResponse()
    resp.ParseFromString(data)
    return resp


def collect_detections(resp):
    result = []
    for output in resp.outputs:
        if output.type != "detection" or not output.HasField("detection"):
            continue
        result.extend(output.detection.detections)
    return result


def draw_detections(src_path, dst_path, detections):
    with Image.open(src_path).convert("RGB") as img:
        draw = ImageDraw.Draw(img)
        width, _ = img.size
        thickness = max(2, width // 240)
        for det in detections:
            box = det.box
            x1 = box.x
            y1 = box.y
            x2 = box.x + box.width
            y2 = box.y + box.height
            for offset in range(thickness):
                draw.rectangle([x1 - offset, y1 - offset, x2 + offset, y2 + offset], outline="red")
        img.save(dst_path, quality=90)


if __name__ == "__main__":
    main()
