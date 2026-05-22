import argparse
import base64
import itertools
import json
import mimetypes
import os
import random
import threading
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


class MqttDeviceClient:
    def __init__(self, broker: str, topic_prefix: str, token: str, device_id: str, edge_node_id: str):
        import paho.mqtt.client as mqtt

        self.mqtt = mqtt
        self.broker = broker
        self.topic = f"{topic_prefix.rstrip('/')}/{device_id}"
        self.token = token
        self.device_id = device_id
        self.edge_node_id = edge_node_id
        self.connected = threading.Event()
        self.connect_errors = []
        host, port = parse_mqtt_broker(broker)
        self.host = host
        self.port = port
        self.client = mqtt.Client(
            mqtt.CallbackAPIVersion.VERSION2,
            client_id=f"simulator-{device_id}",
        )
        self.client.on_connect = self._on_connect
        self.client.on_disconnect = self._on_disconnect

    def _on_connect(self, client, userdata, flags, reason_code, properties):
        code = getattr(reason_code, "value", reason_code)
        if code == 0 or str(reason_code).lower() == "success":
            self.connect_errors.clear()
            self.connected.set()
            return
        self.connect_errors.append(reason_code)

    def _on_disconnect(self, client, userdata, disconnect_flags, reason_code, properties):
        self.connected.clear()

    def connect(self) -> None:
        self.client.loop_start()
        rc = self.client.connect(self.host, self.port, keepalive=30)
        if rc != self.mqtt.MQTT_ERR_SUCCESS:
            raise RuntimeError(f"mqtt connect failed rc={rc}")
        if not self.connected.wait(timeout=10):
            detail = f" reason={self.connect_errors[-1]}" if self.connect_errors else ""
            raise TimeoutError(f"mqtt connect timeout broker={self.broker}{detail}")
        print(f"[{self.device_id}] mqtt connected broker={self.broker} topic={self.topic}")

    def publish_image(self, path: Path) -> None:
        if not self.connected.is_set():
            raise RuntimeError("mqtt client is disconnected")
        content_type = mimetypes.guess_type(path.name)[0] or "application/octet-stream"
        payload = {
            "device_id": self.device_id,
            "edge_node_id": self.edge_node_id,
            "captured_at": datetime.now(timezone.utc).isoformat(),
            "filename": path.name,
            "content_type": content_type,
            "token": self.token,
            "image_base64": base64.b64encode(path.read_bytes()).decode("ascii"),
        }
        result = self.client.publish(self.topic, json.dumps(payload), qos=1)
        result.wait_for_publish(timeout=20)
        if result.rc != self.mqtt.MQTT_ERR_SUCCESS or not result.is_published():
            raise RuntimeError(f"mqtt publish failed rc={result.rc}")
        print(f"[{self.device_id}] {path.name} -> mqtt {self.broker} topic={self.topic}")

    def close(self) -> None:
        try:
            if self.connected.is_set():
                self.client.disconnect()
        finally:
            self.client.loop_stop()


def parse_mqtt_broker(raw: str):
    broker = raw.strip()
    if broker.startswith("tcp://"):
        broker = broker[len("tcp://") :]
    if ":" not in broker:
        return broker, 1883
    host, port = broker.rsplit(":", 1)
    return host, int(port)


def main():
    parser = argparse.ArgumentParser(description="Simulate IoT camera devices uploading images to the platform.")
    parser.add_argument("--device-id", default=os.getenv("DEVICE_ID", "device_001"))
    parser.add_argument("--edge-node-id", default=os.getenv("EDGE_NODE_ID", "edge_001"))
    parser.add_argument("--route", choices=sorted(DEFAULT_TARGETS), default=os.getenv("SIMULATOR_ROUTE", "edge"))
    parser.add_argument("--protocol", choices=["http", "mqtt"], default=os.getenv("SIMULATOR_PROTOCOL", "http"))
    parser.add_argument("--edge-url", default=os.getenv("EDGE_UPLOAD_URL", DEFAULT_TARGETS["edge"]))
    parser.add_argument("--gateway-url", default=os.getenv("GATEWAY_UPLOAD_URL", DEFAULT_TARGETS["gateway"]))
    parser.add_argument("--upload-url", default=os.getenv("UPLOAD_URL", ""))
    parser.add_argument("--mqtt-broker", default=os.getenv("MQTT_BROKER", "tcp://127.0.0.1:1884"))
    parser.add_argument("--mqtt-topic-prefix", default=os.getenv("MQTT_TOPIC_PREFIX", "iot/images"))
    parser.add_argument("--interval", type=float, default=float(os.getenv("UPLOAD_INTERVAL_SECONDS", "3.0")))
    parser.add_argument("--image-dir", default=os.getenv("IMAGE_DIR", "./VOC2028/VOC2028/JPEGImages"))
    parser.add_argument("--token", default=os.getenv("DEVICE_TOKEN", "course-demo-token"))
    parser.add_argument("--once", action="store_true")
    parser.add_argument("--count", type=int, default=0, help="Number of images to upload. 0 means run forever unless --once is set.")
    args = parser.parse_args()

    targets = {
        "edge": args.edge_url,
        "gateway": args.gateway_url,
    }
    upload_url = args.upload_url or targets[args.route]
    if args.protocol == "http":
        print(f"[simulator] protocol=http route={args.route} upload_url={upload_url}")
    else:
        print(f"[simulator] protocol=mqtt broker={args.mqtt_broker} topic_prefix={args.mqtt_topic_prefix}")

    image_dir = Path(args.image_dir)
    ensure_samples(image_dir)
    images = iter_images(image_dir)
    mqtt_client = None
    if args.protocol == "mqtt":
        mqtt_client = MqttDeviceClient(
            args.mqtt_broker,
            args.mqtt_topic_prefix,
            args.token,
            args.device_id,
            args.edge_node_id,
        )
        try:
            mqtt_client.connect()
        except Exception as exc:
            print(f"[{args.device_id}] mqtt connect failed: {exc}")
            mqtt_client.close()
            return

    try:
        limit = 1 if args.once else (args.count if args.count > 0 else None)
        for path in itertools.islice(images, limit):
            try:
                if mqtt_client:
                    mqtt_client.publish_image(path)
                else:
                    upload(upload_url, args.token, args.device_id, args.edge_node_id, path)
            except Exception as exc:
                print(f"[{args.device_id}] upload failed: {exc}")
            if args.once:
                break
            time.sleep(args.interval)
    finally:
        if mqtt_client:
            mqtt_client.close()


if __name__ == "__main__":
    main()
