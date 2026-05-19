import logging
import os
import time
from concurrent import futures
from io import BytesIO
from pathlib import Path
from urllib.parse import urlparse
from urllib.request import Request, urlopen

import grpc
from PIL import Image

from proto.vision.v1 import vision_pb2


SERVICE_NAME = "vision.v1.VisionAnalysisService"
METHOD_NAME = "AnalyzeImage"


def analyze_image(request, context):
    started = time.perf_counter()
    image_uri = request.image_uri
    image_id = request.image_id
    filename = request.filename or Path(urlparse(image_uri).path or image_uri).name

    try:
        with open_image(request) as img:
            rgb = img.convert("RGB")
            tags = generate_tags(rgb, filename)
    except Exception as exc:
        context.abort(grpc.StatusCode.INVALID_ARGUMENT, f"cannot analyze image_uri={image_uri}: {exc}")

    latency_ms = int((time.perf_counter() - started) * 1000)
    return vision_pb2.AnalyzeImageResponse(
        request_id=request.request_id,
        image_id=image_id,
        model="rule-ai-service-v1",
        latency_ms=latency_ms,
        outputs=[
            vision_pb2.AnalysisOutput(
                type="classification",
                classification=vision_pb2.ClassificationResult(
                    labels=[
                        vision_pb2.Label(tag=tag["tag"], confidence=tag["confidence"])
                        for tag in tags
                    ]
                ),
            )
        ],
    )


def open_image(request):
    image_uri = request.image_uri
    parsed = urlparse(image_uri)
    if parsed.scheme in ("http", "https"):
        req = Request(image_uri, headers={"User-Agent": "iot-vision-ai-service/1.0"})
        timeout = float(os.getenv("AI_IMAGE_FETCH_TIMEOUT_SECONDS", "10"))
        with urlopen(req, timeout=timeout) as resp:
            data = resp.read()
        return Image.open(BytesIO(data))

    image_path = resolve_local_image_uri(image_uri)
    return Image.open(image_path)


def resolve_local_image_uri(image_uri):
    if not image_uri:
        raise ValueError("image_uri is required")
    if image_uri.startswith("file://"):
        image_uri = image_uri[len("file://") :]
    if "://" in image_uri:
        provider = os.getenv("STORAGE_PROVIDER", "LOCAL").upper()
        raise ValueError(
            f"unsupported image_uri for STORAGE_PROVIDER={provider}; "
            "expected a local path or http(s) URL"
        )
    path = Path(image_uri)
    if not path.exists():
        raise FileNotFoundError(path)
    if not path.is_file():
        raise ValueError(f"{path} is not a file")
    return path


def generate_tags(img, original_name):
    tags = []
    name = Path(original_name).name.lower()
    keywords = {
        "car": "vehicle",
        "vehicle": "vehicle",
        "road": "road",
        "campus": "campus",
        "factory": "factory",
        "person": "person",
        "door": "entrance",
        "lab": "lab",
        "device": "device",
    }
    for keyword, tag in keywords.items():
        if keyword in name:
            tags.append({"tag": tag, "confidence": 0.92})

    width, height = img.size
    if width > height:
        tags.append({"tag": "wide-view", "confidence": 0.76})
    else:
        tags.append({"tag": "portrait-view", "confidence": 0.74})

    tags.append(dominant_color_tag(img))
    return dedupe_tags(tags)


def dominant_color_tag(img):
    width, height = img.size
    step_x = max(1, width // 40)
    step_y = max(1, height // 40)
    total_r = total_g = total_b = count = 0
    pixels = img.load()

    for y in range(0, height, step_y):
        for x in range(0, width, step_x):
            r, g, b = pixels[x, y]
            total_r += r
            total_g += g
            total_b += b
            count += 1

    if count == 0:
        return {"tag": "unknown-color", "confidence": 0.5}

    r = total_r // count
    g = total_g // count
    b = total_b // count
    if g > r and g > b:
        return {"tag": "green-scene", "confidence": 0.68}
    if b > r and b > g:
        return {"tag": "blue-scene", "confidence": 0.68}
    if r > 170 and g > 150 and b > 120:
        return {"tag": "bright-scene", "confidence": 0.7}
    return {"tag": "mixed-scene", "confidence": 0.62}


def dedupe_tags(tags):
    seen = set()
    result = []
    for tag in tags:
        name = tag["tag"]
        if name in seen:
            continue
        seen.add(name)
        result.append(tag)
    return result


def request_deserializer(data):
    request = vision_pb2.AnalyzeImageRequest()
    request.ParseFromString(data)
    return request


def response_serializer(data):
    return data.SerializeToString()


def serve():
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    port = os.getenv("AI_SERVICE_PORT", "9000")
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=int(os.getenv("AI_SERVICE_WORKERS", "8"))))
    method = grpc.unary_unary_rpc_method_handler(
        analyze_image,
        request_deserializer=request_deserializer,
        response_serializer=response_serializer,
    )
    service = grpc.method_handlers_generic_handler(SERVICE_NAME, {METHOD_NAME: method})
    server.add_generic_rpc_handlers((service,))
    server.add_insecure_port(f"[::]:{port}")
    server.start()
    logging.info("ai-service started on :%s", port)
    server.wait_for_termination()


if __name__ == "__main__":
    serve()
