import argparse
import concurrent.futures
import random
import statistics
import threading
import time
from collections import Counter
from datetime import datetime, timezone
from pathlib import Path

import requests
from PIL import Image, ImageDraw


# 三个内置压测档位：
# - low：轻量验证，确认链路能跑通。
# - mid：中等压力，适合普通电脑做课程演示。
# - high：高压档，用于更高并发和更大图片的压力测试。
PRESETS = {
    "low": {
        "workers": 10,
        "duration": 30,
        "image_count": 30,
        "width": 640,
        "height": 360,
        "quality": 82,
    },
    "mid": {
        "workers": 50,
        "duration": 60,
        "image_count": 100,
        "width": 1280,
        "height": 720,
        "quality": 86,
    },
    "high": {
        "workers": 100,
        "duration": 120,
        "image_count": 200,
        "width": 1920,
        "height": 1080,
        "quality": 88,
    },
}

TARGETS = {
    "edge": "http://127.0.0.1:8081/api/edge/upload",
    "gateway": "http://127.0.0.1:5173/api/images/upload",
}


class Metrics:
    """线程安全的压测指标收集器。

    多个上传线程会同时记录结果，所以这里用 lock 保护 total/success/latencies 等共享数据。
    """

    def __init__(self):
        self.lock = threading.Lock()
        self.total = 0
        self.success = 0
        self.failed = 0
        self.latencies = []
        self.statuses = Counter()
        self.errors = Counter()

    def record(self, status_code=None, latency=None, error=None):
        """记录一次请求结果。

        status_code 为 HTTP 状态码；latency 为本次请求耗时；error 为异常类型。
        2xx 视为成功，其他状态码或异常都视为失败。
        """
        with self.lock:
            self.total += 1
            if error is None and status_code is not None and 200 <= status_code < 300:
                self.success += 1
            else:
                self.failed += 1
            if status_code is not None:
                self.statuses[str(status_code)] += 1
            if latency is not None:
                self.latencies.append(latency)
            if error:
                self.errors[str(error)[:120]] += 1

    def snapshot(self):
        """拷贝一份当前指标，避免打印统计时长时间持有锁。"""
        with self.lock:
            return {
                "total": self.total,
                "success": self.success,
                "failed": self.failed,
                "latencies": list(self.latencies),
                "statuses": self.statuses.copy(),
                "errors": self.errors.copy(),
            }


def percentile(values, pct):
    """计算百分位延迟，例如 pct=0.95 表示 P95。"""
    if not values:
        return 0.0
    ordered = sorted(values)
    index = int((len(ordered) - 1) * pct)
    return ordered[index]


def summarize(metrics, started_at):
    """把原始指标转换成报告中常用的 QPS、成功 QPS、平均延迟、P95/P99 延迟。"""
    snap = metrics.snapshot()
    elapsed = max(time.perf_counter() - started_at, 0.001)
    latencies = snap["latencies"]
    avg = statistics.mean(latencies) if latencies else 0.0
    return {
        "elapsed": elapsed,
        "total": snap["total"],
        "success": snap["success"],
        "failed": snap["failed"],
        "qps": snap["total"] / elapsed,
        "success_qps": snap["success"] / elapsed,
        "avg_ms": avg * 1000,
        "p95_ms": percentile(latencies, 0.95) * 1000,
        "p99_ms": percentile(latencies, 0.99) * 1000,
        "statuses": snap["statuses"],
        "errors": snap["errors"],
    }


def print_summary(title, metrics, started_at):
    """打印最终压测结果。"""
    data = summarize(metrics, started_at)
    print(f"\n[{title}]")
    print(f"elapsed:      {data['elapsed']:.1f}s")
    print(f"total:        {data['total']}")
    print(f"success:      {data['success']}")
    print(f"failed:       {data['failed']}")
    print(f"qps:          {data['qps']:.2f}/s")
    print(f"success qps:  {data['success_qps']:.2f}/s")
    print(f"avg latency:  {data['avg_ms']:.1f} ms")
    print(f"p95 latency:  {data['p95_ms']:.1f} ms")
    print(f"p99 latency:  {data['p99_ms']:.1f} ms")
    print(f"status codes: {dict(data['statuses'])}")
    if data["errors"]:
        print(f"errors:       {dict(data['errors'].most_common(5))}")


def ensure_images(image_dir, image_count, width, height, quality, force=False):
    """生成压测用 JPEG 图片。

    如果目录里已有足够数量的 pressure_*.jpg，会直接复用，避免每次压测都重新生成。
    图片内容会加入随机线条和文字，既能模拟视觉数据，也能避免所有文件完全相同。
    """
    image_dir.mkdir(parents=True, exist_ok=True)
    existing = sorted(image_dir.glob("pressure_*.jpg"))
    if not force and len(existing) >= image_count:
        return existing[:image_count]

    print(f"Generating {image_count} test images in {image_dir} ...")
    palette = [
        (68, 132, 206),
        (81, 154, 116),
        (205, 117, 84),
        (151, 111, 194),
        (214, 172, 78),
        (72, 176, 177),
    ]
    for idx in range(image_count):
        base = palette[idx % len(palette)]
        img = Image.new("RGB", (width, height), base)
        draw = ImageDraw.Draw(img)
        random.seed(idx * 7919 + width + height)

        for _ in range(36):
            color = tuple(max(0, min(255, c + random.randint(-55, 70))) for c in base)
            x1 = random.randint(0, width - 1)
            y1 = random.randint(0, height - 1)
            x2 = random.randint(x1, width)
            y2 = random.randint(y1, height)
            if random.random() > 0.5:
                draw.rectangle((x1, y1, x2, y2), outline=color, width=random.randint(2, 8))
            else:
                draw.line((x1, y1, x2, y2), fill=color, width=random.randint(2, 10))

        draw.rectangle((32, 32, width - 32, height - 32), outline=(255, 255, 255), width=4)
        draw.text((56, 56), f"IoT pressure image {idx:04d}", fill=(255, 255, 255))
        img.save(image_dir / f"pressure_{idx:04d}.jpg", "JPEG", quality=quality, optimize=True)

    return sorted(image_dir.glob("pressure_*.jpg"))[:image_count]


def upload_once(session, target, token, path, device_id, edge_node_id, timeout):
    """上传单张图片。

    使用 multipart/form-data，字段与 edge-node/cloud-api 的上传接口保持一致：
    file、device_id、edge_node_id、captured_at，以及请求头 X-Device-Token。
    """
    with path.open("rb") as fh:
        files = {"file": (path.name, fh, "image/jpeg")}
        data = {
            "device_id": device_id,
            "edge_node_id": edge_node_id,
            "captured_at": datetime.now(timezone.utc).isoformat(),
        }
        started = time.perf_counter()
        resp = session.post(
            target,
            headers={"X-Device-Token": token},
            files=files,
            data=data,
            timeout=timeout,
        )
        latency = time.perf_counter() - started
    return resp.status_code, latency


def worker_loop(worker_id, args, target, images, stop_at, metrics):
    """单个压测线程的循环。

    每个线程模拟一台设备，不断从生成的图片集中随机选图并上传，直到到达 stop_at。
    """
    session = requests.Session()
    rng = random.Random(worker_id * 1009)
    sent = 0
    while time.perf_counter() < stop_at:
        path = rng.choice(images)
        device_id = f"{args.device_prefix}_{worker_id:03d}"
        try:
            status_code, latency = upload_once(
                session=session,
                target=target,
                token=args.token,
                path=path,
                device_id=device_id,
                edge_node_id=args.edge_node_id,
                timeout=args.timeout,
            )
            metrics.record(status_code=status_code, latency=latency)
        except Exception as exc:
            metrics.record(error=type(exc).__name__)
        sent += 1
        if args.sleep > 0:
            time.sleep(args.sleep)
    return sent


def parse_args():
    """解析命令行参数。

    默认用 --low；也可以通过 --workers、--duration、--image-count 等参数覆盖档位配置。
    """
    parser = argparse.ArgumentParser(description="Pressure test IoT Vision upload API with generated images.")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--low", action="store_true", help="10 workers, 30 seconds, small images.")
    mode.add_argument("--mid", action="store_true", help="50 workers, 60 seconds, medium images.")
    mode.add_argument("--high", action="store_true", help="100 workers, 120 seconds, large images.")

    parser.add_argument(
        "--route",
        choices=sorted(TARGETS),
        default="edge",
        help="Upload route: edge uses device -> edge-node -> nginx -> cloud-api; gateway uploads directly through nginx.",
    )
    parser.add_argument("--target", help="Override upload URL. Defaults to --route edge/gateway.")
    parser.add_argument("--token", default="course-demo-token")
    parser.add_argument("--edge-node-id", default="edge_pressure")
    parser.add_argument("--device-prefix", default="pressure_device")
    parser.add_argument("--image-dir", default="pressure-test/generated-images")
    parser.add_argument("--workers", type=int)
    parser.add_argument("--duration", type=int)
    parser.add_argument("--image-count", type=int)
    parser.add_argument("--width", type=int)
    parser.add_argument("--image-height", type=int)
    parser.add_argument("--quality", type=int)
    parser.add_argument("--timeout", type=float, default=30.0)
    parser.add_argument("--sleep", type=float, default=0.0, help="Optional sleep seconds after every request per worker.")
    parser.add_argument("--regenerate-images", action="store_true")
    return parser.parse_args()


def choose_preset(args):
    """根据命令行选择压测档位，并应用用户传入的覆盖参数。"""
    if args.high:
        name = "high"
    elif args.mid:
        name = "mid"
    else:
        name = "low"

    preset = PRESETS[name].copy()
    if args.workers is not None:
        preset["workers"] = args.workers
    if args.duration is not None:
        preset["duration"] = args.duration
    if args.image_count is not None:
        preset["image_count"] = args.image_count
    if args.width is not None:
        preset["width"] = args.width
    if args.image_height is not None:
        preset["height"] = args.image_height
    if args.quality is not None:
        preset["quality"] = args.quality
    return name, preset


def resolve_target(args):
    if args.target:
        return args.target
    return TARGETS[args.route]


def main():
    """压测入口：

    1. 解析参数并选择档位；
    2. 自动生成或复用测试图片；
    3. 创建线程池并发上传；
    4. 每 5 秒打印进度；
    5. 输出最终 QPS 和延迟指标。
    """
    args = parse_args()
    preset_name, preset = choose_preset(args)
    target = resolve_target(args)
    image_dir = Path(args.image_dir)
    images = ensure_images(
        image_dir=image_dir,
        image_count=preset["image_count"],
        width=preset["width"],
        height=preset["height"],
        quality=preset["quality"],
        force=args.regenerate_images,
    )

    print("IoT Vision pressure upload")
    print(f"mode:         {preset_name}")
    print(f"route:        {args.route}")
    print(f"target:       {target}")
    print(f"workers:      {preset['workers']}")
    print(f"duration:     {preset['duration']}s")
    print(f"images:       {len(images)} files, {preset['width']}x{preset['height']}")
    print(f"image dir:    {image_dir.resolve()}")
    if args.route == "edge":
        print("Tip: use start-docker-pressure.cmd first so edge deduplication is disabled.")
    else:
        print("Tip: gateway route bypasses edge-node and uploads directly to cloud-api through nginx.")
    print()

    metrics = Metrics()
    started_at = time.perf_counter()
    stop_at = started_at + preset["duration"]
    next_report = started_at + 5

    with concurrent.futures.ThreadPoolExecutor(max_workers=preset["workers"]) as executor:
        futures = [
            executor.submit(worker_loop, idx + 1, args, target, images, stop_at, metrics)
            for idx in range(preset["workers"])
        ]
        while time.perf_counter() < stop_at:
            time.sleep(0.5)
            now = time.perf_counter()
            if now >= next_report:
                data = summarize(metrics, started_at)
                print(
                    f"[progress] {data['elapsed']:.1f}s total={data['total']} "
                    f"ok={data['success']} fail={data['failed']} qps={data['qps']:.2f}/s "
                    f"p95={data['p95_ms']:.1f}ms"
                )
                next_report += 5
        concurrent.futures.wait(futures)

    print_summary("final", metrics, started_at)


if __name__ == "__main__":
    main()
