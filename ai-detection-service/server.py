import logging
import os
import sys
import tempfile
import time
from concurrent import futures
from pathlib import Path
from urllib.parse import urlparse
from urllib.request import Request, urlopen

import grpc
from PIL import Image


REPO_ROOT = Path(__file__).resolve().parents[1]
AI_SERVICE_PROTO_ROOT = REPO_ROOT / "ai-service"
if str(AI_SERVICE_PROTO_ROOT) not in sys.path:
    sys.path.insert(0, str(AI_SERVICE_PROTO_ROOT))

from proto.vision.v1 import vision_pb2  # noqa: E402


SERVICE_NAME = "vision.v1.VisionAnalysisService"
METHOD_NAME = "AnalyzeImage"
MODEL_ID = os.getenv("DETECTION_MODEL_ID", "iic/cv_tinynas_head-detection_damoyolo")


class DetectionModel:
    def __init__(self):
        import torch
        from modelscope.pipelines import pipeline
        from modelscope.utils.constant import Tasks

        if not torch.cuda.is_available():
            raise RuntimeError("CUDA is not available. This service is configured for GPU inference only.")
        device_name = torch.cuda.get_device_name(0)
        logging.info("CUDA available: %s", device_name)

        device = os.getenv("DETECTION_DEVICE", "gpu")
        logging.info("loading detection model=%s device=%s", MODEL_ID, device)
        self.pipeline = pipeline(
            Tasks.domain_specific_object_detection,
            model=MODEL_ID,
            device=device,
            trust_remote_code=True,
        )
        self.threshold = float(os.getenv("DETECTION_SCORE_THRESHOLD", "0.35"))
        self.default_label = os.getenv("DETECTION_DEFAULT_LABEL", "head")

    def predict(self, image_path):
        raw = self.pipeline(str(image_path))
        return parse_detections(raw, self.threshold, self.default_label)


model = None


def get_model():
    global model
    if model is None:
        model = DetectionModel()
    return model


def analyze_image(request, context):
    started = time.perf_counter()
    if not wants_detection(request):
        context.abort(grpc.StatusCode.INVALID_ARGUMENT, "expected task type=detection")

    temp_path = None
    try:
        image_path, temp_path = prepare_image_path(request.image_uri)
        with Image.open(image_path) as img:
            img.verify()
        detections = get_model().predict(image_path)
    except Exception as exc:
        context.abort(grpc.StatusCode.INVALID_ARGUMENT, f"cannot detect image_uri={request.image_uri}: {exc}")
    finally:
        if temp_path:
            try:
                Path(temp_path).unlink(missing_ok=True)
            except Exception:
                logging.exception("failed to delete temp image %s", temp_path)

    latency_ms = int((time.perf_counter() - started) * 1000)
    return vision_pb2.AnalyzeImageResponse(
        request_id=request.request_id,
        image_id=request.image_id,
        model="modelscope-cv_tinynas_head-detection_damoyolo",
        latency_ms=latency_ms,
        outputs=[
            vision_pb2.AnalysisOutput(
                type="detection",
                detection=vision_pb2.DetectionResult(detections=detections),
            )
        ],
    )


def wants_detection(request):
    if not request.tasks:
        return True
    return any(task.type == "detection" for task in request.tasks)


def prepare_image_path(image_uri):
    if not image_uri:
        raise ValueError("image_uri is required")
    parsed = urlparse(image_uri)
    if parsed.scheme in ("http", "https"):
        timeout = float(os.getenv("DETECTION_IMAGE_FETCH_TIMEOUT_SECONDS", "10"))
        req = Request(image_uri, headers={"User-Agent": "iot-vision-detection-service/1.0"})
        with urlopen(req, timeout=timeout) as resp:
            data = resp.read()
        suffix = Path(parsed.path).suffix or ".jpg"
        handle = tempfile.NamedTemporaryFile(delete=False, suffix=suffix)
        try:
            handle.write(data)
            return Path(handle.name), handle.name
        finally:
            handle.close()

    image_path = image_uri
    if image_path.startswith("file://"):
        image_path = image_path[len("file://") :]
    path = Path(image_path)
    if not path.exists():
        raise FileNotFoundError(path)
    if not path.is_file():
        raise ValueError(f"{path} is not a file")
    return path, None


def parse_detections(raw, threshold, default_label):
    if isinstance(raw, list):
        if not raw:
            return []
        raw = raw[0]
    boxes = get_value(raw, "boxes", "bboxes", "boxes_xyxy")
    scores = get_value(raw, "scores", "score", "confidences")
    labels = get_value(raw, "labels", "label", "classes")
    if boxes is None:
        return []
    boxes = to_list(boxes)
    scores = to_list(scores) if scores is not None else [1.0] * len(boxes)
    labels = to_list(labels) if labels is not None else [default_label] * len(boxes)

    detections = []
    for index, box in enumerate(boxes):
        box = to_list(box)
        if len(box) < 4:
            continue
        score = float(scores[index]) if index < len(scores) else 1.0
        if score < threshold:
            continue
        label = str(labels[index]) if index < len(labels) else default_label
        x1, y1, x2, y2 = [float(v) for v in box[:4]]
        if x2 < x1:
            x1, x2 = x2, x1
        if y2 < y1:
            y1, y2 = y2, y1
        detections.append(
            vision_pb2.Detection(
                label=label or default_label,
                confidence=score,
                box=vision_pb2.BoundingBox(
                    x=x1,
                    y=y1,
                    width=max(0.0, x2 - x1),
                    height=max(0.0, y2 - y1),
                    coordinate_type="pixel",
                ),
            )
        )
    return detections


def get_value(mapping, *names):
    for name in names:
        if isinstance(mapping, dict) and name in mapping:
            return mapping[name]
    return None


def to_list(value):
    if value is None:
        return []
    if hasattr(value, "tolist"):
        return value.tolist()
    if isinstance(value, tuple):
        return list(value)
    if isinstance(value, list):
        return value
    return [value]


def request_deserializer(data):
    request = vision_pb2.AnalyzeImageRequest()
    request.ParseFromString(data)
    return request


def response_serializer(data):
    return data.SerializeToString()


def serve():
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    port = os.getenv("AI_DETECTION_PORT", "9100")
    get_model()
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=int(os.getenv("AI_DETECTION_WORKERS", "2"))))
    method = grpc.unary_unary_rpc_method_handler(
        analyze_image,
        request_deserializer=request_deserializer,
        response_serializer=response_serializer,
    )
    service = grpc.method_handlers_generic_handler(SERVICE_NAME, {METHOD_NAME: method})
    server.add_generic_rpc_handlers((service,))
    server.add_insecure_port(f"[::]:{port}")
    server.start()
    logging.info("ai-detection-service started on :%s", port)
    server.wait_for_termination()


if __name__ == "__main__":
    serve()
