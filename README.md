# 基于云边端协同的物联网视觉数据平台

这是一个用于《物联网技术与应用》课程报告的 MVP 项目，覆盖端侧采集、边缘去重、云端存储、RabbitMQ 队列处理、Redis 短 TTL 查询缓存、Worker 异步图片处理、MySQL 元数据管理和 Vue 可视化展示。

## 目录结构

```text
iot-vision-platform/
├── cmd/
│   ├── cloud-api/      # 云端 API 服务
│   ├── edge-node/      # 边缘节点服务
│   └── worker/         # RabbitMQ 图片处理 Worker
├── device-simulator/   # Python 设备模拟器
├── frontend/           # Vue3 前端
├── deploy/mysql/       # MySQL 初始化 SQL
├── internal/platform/  # Go 共享配置、模型、图片处理工具
├── storage/            # 运行时图片和边缘缓存目录
├── docker-compose.yml  # Docker 编排配置
└── .env.example
```

## 数据流说明

平台有两条核心数据流：一条是端侧设备上传图片并触发云端异步处理，另一条是用户通过 Web 前端查看图片和元数据。`STORAGE_PROVIDER` 控制业务图片实际存储位置：`LOCAL` 时使用本地 `storage` 目录，`HUAWEI` 时使用华为云 OBS。edge-node 的失败重传缓存始终保留在本地 `storage/edge-cache`，用于保证边缘节点在云端短暂不可用时仍能接收设备数据。

### 端侧设备上传图片

```text
device-simulator / pressure-test
  -> HTTP edge-node :8081 /api/edge/upload
  -> MQTT mqtt-broker :1883 iot/images/{device_id}
  -> edge-node MQTT consumer
  -> nginx gateway(frontend :5173) /api/images/upload
  -> cloud-api :8080
  -> Storage(LOCAL 或 HUAWEI OBS)
  -> MySQL images/devices
  -> RabbitMQ image.process
  -> worker
  -> Storage 读取原图、写入缩略图或红框检测图
  -> ai-detection-service(可选，本机 GPU) 读取原图并返回检测框
  -> MySQL image_tags/images 状态更新
```

1. 端侧设备可以通过 HTTP multipart 上传图片到 `edge-node`，也可以通过 MQTT 发布 JSON + base64 单消息到 `iot/images/{device_id}`。
2. HTTP 请求通过 `X-Device-Token` 鉴权；MQTT 消息通过 JSON payload 中的 `token` 字段鉴权。两种入口都会提交 `device_id`、`edge_node_id`、`captured_at` 等字段。
3. `edge-node` 校验设备 token，计算图片 SHA256。启用 `EDGE_DEDUP_ENABLED=true` 时，重复图片会在边缘侧直接过滤，减少云端压力。
4. `edge-node` 把图片转发到 `cloud-api`。如果云端暂时不可用，图片和元数据会写入本地 `storage/edge-cache`，后台重试循环会定时重新转发。
5. `cloud-api` 再次校验设备 token，并通过统一 `Storage` 接口保存原图：
   - `STORAGE_PROVIDER=LOCAL`：写入本地 `storage/original/...`。
   - `STORAGE_PROVIDER=HUAWEI`：写入华为 OBS 的 `original/...` 对象。
6. `cloud-api` 在 MySQL 中创建图片记录，状态为 `queued`，同时更新或创建设备记录。`images.original_path` 保存当前存储中的 key，OBS 模式下还会写入 `original_storage_provider`、`original_bucket`、`original_object_key`、`original_object_url` 等字段。
7. `cloud-api` 向 RabbitMQ `image.process` 队列写入持久化任务消息，消息中包含 `image_id`、`original_path`、`device_id`、`edge_node_id` 等信息。如果 RabbitMQ 启动时不可用，worker 会降级轮询 MySQL 中的 `queued` 任务。
8. `worker` 消费任务后把图片状态改为 `processing`，通过统一 `Storage.Get` 读取原图，不再直接依赖本地文件路径。
9. `worker` 解码图片、计算 hash、读取尺寸和格式，并生成 JPEG 缩略图。启用本机 GPU `ai-detection-service` 时，worker 会调用目标检测服务获取检测框，并把红框画到缩略图上。缩略图通过统一 `Storage.Put` 写入：
   - `LOCAL`：本地 `storage/thumbnail/...`。
   - `HUAWEI`：OBS 的 `thumbnail/...` 对象。
10. `worker` 使用 Go 本地规则生成标签。目标检测服务不可用、超时或没有返回检测框时，worker 会降级生成普通缩略图，保证图片任务仍能完成。
11. `worker` 把标签写入 `image_tags`，并更新 `images` 表中的 `thumbnail_path`、`width`、`height`、`format`、`size` 和状态 `completed`。失败时状态会变为 `failed`，并写入 `error_message`。

### Web 前端查看图片

```text
browser(Vue :5173)
  -> nginx gateway(frontend :5173) /api/auth/login
  -> browser localStorage 保存 JWT
  -> nginx gateway(frontend :5173) /api/images /api/devices /api/tasks/status /api/stats
  -> cloud-api :8080
  -> Redis 查询缓存(/devices /tasks/status /stats)
  -> MySQL 查询元数据(缓存未命中时)
  -> cloud-api 生成 original_url/thumbnail_url
  -> browser <img> 直接请求本地静态文件或 OBS URL
```

1. 用户打开 Vue 前端并登录，前端调用 `POST /api/auth/login`。登录成功后，JWT token 保存在浏览器 `localStorage`。
2. 前端后续请求会自动携带 `Authorization: Bearer <token>`，访问 `/api/images`、`/api/devices`、`/api/tasks/status`、`/api/stats`。
3. `cloud-api` 校验 JWT 后读取图片、设备、任务和统计元数据，并把图片记录转换为前端需要的响应结构。其中 `/api/devices`、`/api/tasks/status`、`/api/stats` 会先读 Redis 短 TTL 缓存，未命中时再回源 MySQL。
4. 对每张图片，`cloud-api` 会返回：
   - `original_url`：原图访问地址。
   - `thumbnail_url`：缩略图访问地址。
5. `STORAGE_PROVIDER=LOCAL` 时，URL 形如 `/files/original/...` 或 `/files/thumbnail/...`，前端通过同源 nginx 网关访问，由 nginx 转发到 `cloud-api` 的静态文件路由返回图片。
6. `STORAGE_PROVIDER=HUAWEI` 时，URL 指向 OBS：
   - 如果配置了 `HUAWEI_OBS_PUBLIC_URL`，后端优先返回公开访问 URL。
   - 如果没有配置公开 URL，后端会生成临时签名 URL，适合私有桶。
7. 前端用 `<img>` 直接加载 `thumbnail_url` 显示图库缩略图；用户点击图片后，弹窗使用 `original_url` 加载原图，并展示设备、标签、尺寸、大小、格式和采集时间。
8. 前端每 5 秒刷新一次数据，因此图片状态会从 `queued`、`processing` 自动变为 `completed`，缩略图和标签也会在 worker 处理完成后出现在页面上。

### RabbitMQ 队列与 Redis 缓存层

当前项目把异步任务队列和查询缓存拆开：

- 消息队列：`cloud-api` 上传成功后向 RabbitMQ `image.process` 写入图片处理任务，worker 通过 manual ack 和 `prefetch=1` 并行消费。
- 查询缓存：`cloud-api` 对高频聚合接口使用短 TTL JSON 缓存，降低前端轮询时对 MySQL 的重复查询压力。

当前缓存范围：

| 接口 | Redis key | TTL |
| --- | --- | --- |
| `GET /api/stats` | `cache:cloud-api:stats` | 5 秒 |
| `GET /api/tasks/status` | `cache:cloud-api:tasks_status` | 2 秒 |
| `GET /api/devices` | `cache:cloud-api:devices` | 10 秒 |
| 默认第一页 `GET /api/images` | `cache:cloud-api:images:first_page` | 2 秒 |

`GET /api/images` 只缓存前端默认第一页：无筛选条件、`page=1`、`page_size=60`、`sort_by=created_at`、`sort_order=desc`。带设备、状态、标签、时间范围筛选，或翻页、改排序时仍直接查询 MySQL，避免缓存 key 爆炸和复杂失效问题。

缓存一致性采用“短 TTL + 上传后主动失效”的方式：`cloud-api` 每次成功接收新图片后会删除以上缓存 key，让新增图片和设备尽快体现在前端；worker 更新任务状态和标签时不主动操作 cloud-api 缓存，依赖 `/api/tasks/status` 与默认图片列表的短 TTL 快速收敛。Redis 不可用时，接口会自动回退到 MySQL 查询，核心上传和展示链路仍可运行。

RabbitMQ 使用 direct exchange `image.tasks`、主队列 `image.process` 和固定延迟 retry queue。worker 只有在处理完成或业务失败已写入 MySQL 后才 ack；可重试数据库错误会进入 retry queue，默认 5 秒后回到主队列，超过 3 次后图片会被标记为 `failed`。

`/api/tasks/status` 还会顺带清理异常中断留下的孤儿任务：如果某张图片保持 `processing` 超过 10 分钟，说明 worker 很可能在处理过程中被停止或重启，`cloud-api` 会把这类记录标记为 `failed` 并写入超时错误信息，避免前端任务状态长期卡在“处理中”。

多个 worker 高并发写入 MySQL 时，`image_tags` 删除、插入和 `images` 状态更新可能遇到 MySQL 死锁 `Error 1213 (40001)` 或锁等待超时。worker 会对这类可重试数据库错误自动重试事务；如果数据库暂时不可写，RabbitMQ 消息不会被 ack，而是进入 retry queue 延迟重试。worker 进程异常退出时，未 ack 的消息会自动回到队列，避免任务静默丢失。

## 环境要求

Docker 部署只需要：

- Docker Desktop / Docker Compose v2

本机手动部署需要：

- Windows + PowerShell
- MySQL 8.0
- Go 1.26+
- Conda / Python 3.12
- Docker，用于启动 Redis
- Node.js 22+，前端命令请使用 `npm.cmd`

## Docker Compose 一键部署

如果使用 Docker 部署，项目会把 MySQL、Redis、云端 API、Worker、边缘节点和前端全部放进容器，不再依赖本机 MySQL / WSL Redis。

启动核心服务：

```powershell
cd D:\Code\Iot\Final\iot-vision-platform
.\start-docker.cmd
```

压测时如果样例图片数量不够，可以临时关闭边缘节点 SHA256 去重，让重复图片也继续进入 cloud-api、Redis、Worker 和 MySQL：

```powershell
.\start-docker-pressure.cmd
```

也可以手动指定：

```powershell
$env:EDGE_DEDUP_ENABLED="false"
docker compose up --build -d edge-node
```

确认开关状态：

```powershell
curl http://127.0.0.1:8081/api/edge/status
```

返回中 `dedup_enabled` 为 `false` 时，重复图片不会被边缘节点过滤。

## Python 压测脚本

项目提供了 Python 自动压测脚本，会自动生成 JPEG 测试图片，并发上传到 edge-node：

```powershell
cd D:\Code\Iot\Final\iot-vision-platform
pip install -r pressure-test/requirements.txt
```

压测前建议先用压测模式启动 Docker 服务：

```powershell
.\start-docker-pressure.cmd
```

三个默认档位：

```powershell
python pressure-test/pressure_upload.py --low --route edge
python pressure-test/pressure_upload.py --mid --route edge
python pressure-test/pressure_upload.py --high --route edge
```

也可以用批处理脚本：

```powershell
.\run-pressure-low.cmd
.\run-pressure-mid.cmd
.\run-pressure-high.cmd
```

档位含义：

| 档位 | 并发线程 | 持续时间 | 图片尺寸 | 图片数量 |
| --- | ---: | ---: | ---: | ---: |
| `--low` | 10 | 30 秒 | 640x360 | 30 |
| `--mid` | 50 | 60 秒 | 1280x720 | 100 |
| `--high` | 100 | 120 秒 | 1920x1080 | 200 |

压测输出中的 `qps` 就是整体请求吞吐，`success qps` 是成功上传吞吐，`p95 latency` 和 `p99 latency` 可用于报告中的延迟指标。

自定义参数示例：

```powershell
python pressure-test/pressure_upload.py --mid --workers 80 --duration 90 --image-count 150
```

如果要直压 cloud-api 而不是完整边缘链路：

```powershell
python pressure-test/pressure_upload.py --low --route gateway
```

`--route edge` 会走完整链路 `设备 -> edge-node -> nginx -> cloud-api`；`--route gateway` 会绕过 edge-node，直接通过 nginx 网关上传到 `cloud-api`，适合单独压测 cloud-api 横向扩展能力。也可以用 `--target` 手动覆盖上传 URL。

## 查询接口压测

如果要压测前端总览和图库相关查询效率，可以先自行安装 `hey`，然后运行：

```powershell
.\run-query-pressure.cmd
```

脚本会自动登录 `http://127.0.0.1:5173/api/auth/login` 获取 JWT，并依次压测四个接口：

```text
GET /api/stats
GET /api/tasks/status
GET /api/devices
GET /api/images?page=1&page_size=60&sort_by=created_at&sort_order=desc
```

四个接口都会命中当前缓存路径，其中 `/api/images` 只缓存默认第一页。默认参数是并发 50、持续 60 秒，并会先请求一次四个接口用于热缓存。如果想观察 MySQL 原始查询压力，可以加 `-NoWarmup` 或压测带筛选、翻页、改排序的 `/api/images` 请求。

自定义参数示例：

```powershell
.\run-query-pressure.cmd -Concurrency 100 -Duration 120s
.\run-query-pressure.cmd -BaseUrl http://127.0.0.1:5173 -Username admin -Password admin123456
.\run-query-pressure.cmd -NoWarmup
```

压测报告里重点看 `Requests/sec`、平均延迟、`95%`、`99%` 和非 2xx 响应数量。通常缓存接口的 QPS 应明显高于 `/api/images`。

一次本地查询压测记录，记录于默认图片列表缓存加入之前：

```text
时间：2026-05-21
入口：http://127.0.0.1:5173
工具：hey
参数：并发 50，每个接口持续 60 秒
结果：四个接口均为 200 响应，无非 2xx 错误

/api/stats          QPS 3688.6，平均 13.6ms，P95 37.5ms，P99 56.5ms
/api/tasks/status   QPS 3165.3，平均 15.8ms，P95 38.5ms，P99 65.1ms
/api/devices        QPS 3526.6，平均 14.2ms，P95 35.7ms，P99 51.2ms
/api/images         QPS 255.8， 平均 195.3ms，P95 355.5ms，P99 435.4ms
```

结论：`/api/stats`、`/api/tasks/status`、`/api/devices` 三个 Redis 缓存接口 QPS 均超过 3000，延迟较低；当时未缓存的 `/api/images` 主要依赖 MySQL 分页、排序、标签预加载和图片 URL 拼接，是查询链路瓶颈。后续已对默认第一页 `/api/images` 增加 2 秒 Redis 缓存，并为 `images.created_at`、`status + created_at`、`device_id + created_at` 增加索引。

默认 Dockerfile 使用 `docker.m.daocloud.io/library` 作为基础镜像源，避免直接访问 Docker Hub 超时。如果你的网络可以直连 Docker Hub，也可以把 `docker-compose.yml` 中的 `IMAGE_REGISTRY` 改成 `docker.io/library`。

等容器启动后打开：

```text
http://127.0.0.1:5173
```

默认登录账号：

```text
username: admin
password: admin123456
```

查看服务状态：

```powershell
docker compose ps
```

查看日志：

```powershell
docker compose logs -f frontend
docker compose logs -f cloud-api
docker compose logs -f worker
docker compose logs -f edge-node
```

横向扩展 `cloud-api`：

```powershell
docker compose up -d --scale cloud-api=3 cloud-api frontend
```

如果扩容后 nginx 没有立刻识别新的 `cloud-api` 副本，可以重启 frontend 容器刷新 Docker DNS 解析结果：

```powershell
docker compose restart frontend
```

停止 Docker 版本：

```powershell
.\stop-docker.cmd
```

如果需要彻底清空 Docker 数据库、Redis 和图片文件：

```powershell
docker compose down -v
```

Docker 版本默认端口：

```text
frontend/nginx gateway: http://127.0.0.1:5173
API health:             http://127.0.0.1:5173/api/health
edge-node:              http://127.0.0.1:8081
mqtt broker:            tcp://127.0.0.1:1884
mysql:                  127.0.0.1:3307
redis:                  127.0.0.1:6379
rabbitmq:               amqp://127.0.0.1:5672
rabbitmq management:    http://127.0.0.1:15672
```

Docker 模式下 `cloud-api` 不再默认暴露宿主机 `8080`，只在 Compose 内部网络中监听 `8080`，由 frontend 容器内的 nginx 统一代理 `/api/*` 和 `/files/*`。

Docker 版本还包含一个 Python `ai-service`，Worker 会通过 gRPC 调用它进行标签分析。`STORAGE_PROVIDER=LOCAL` 时它可继续通过只读共享 volume 读取 `/app/storage/original/...` 图片路径；`STORAGE_PROVIDER=HUAWEI` 时 Worker 会传入华为 OBS 的可访问 URL，`ai-service` 不再依赖本地原图文件。

注意：Docker 内部服务仍通过 `mysql:3306` 通信；为了避免和本机 MySQL 冲突，默认把容器 MySQL 映射到宿主机 `3307`。

### 登录认证

平台管理端使用 JWT 登录认证。cloud-api 首次启动时会自动检查并创建默认管理员账号，账号密码可通过环境变量修改：

```text
JWT_SECRET=course-demo-jwt-secret
JWT_EXPIRE_HOURS=24
DEFAULT_ADMIN_USERNAME=admin
DEFAULT_ADMIN_PASSWORD=admin123456
```

前端登录成功后会把 token 保存到浏览器 `localStorage`，后续请求自动携带：

```http
Authorization: Bearer <token>
```

设备上传不使用浏览器 JWT，仍然走独立的 `X-Device-Token`，方便说明“端侧设备认证”和“管理后台用户认证”是两条不同链路。

## 1. 初始化 MySQL

由于本机 `mysql` 命令可能不在 PATH，推荐用 MySQL Workbench、DataGrip、Navicat 或 MySQL Installer 提供的命令行工具执行：

```sql
source D:/Code/Iot/Final/iot-vision-platform/deploy/mysql/init.sql;
```

默认数据库和账号：

```text
database: iot_vision
user: iot_user
password: iot_password
port: 3306
```

## 2. 配置环境变量

复制 `.env.example` 为 `.env`，按本机 MySQL 密码修改 `MYSQL_DSN`。

```powershell
Copy-Item .env.example .env
```

默认配置：

```text
MYSQL_DSN=iot_user:iot_password@tcp(127.0.0.1:3306)/iot_vision?charset=utf8mb4&parseTime=True&loc=Local
REDIS_ADDR=127.0.0.1:6379
STORAGE_ROOT=./storage
STORAGE_PROVIDER=LOCAL
STORAGE_OBJECT_PREFIX=original
DEVICE_TOKEN=course-demo-token
EDGE_DEDUP_ENABLED=true
CLOUD_API_ADDR=:8080
EDGE_NODE_ADDR=:8081
CLOUD_UPLOAD_URL=http://127.0.0.1:8080/api/images/upload
AI_IMAGE_FETCH_TIMEOUT_SECONDS=10
JWT_SECRET=course-demo-jwt-secret
JWT_EXPIRE_HOURS=24
DEFAULT_ADMIN_USERNAME=admin
DEFAULT_ADMIN_PASSWORD=admin123456
```

## 3. 启动 Redis 和 RabbitMQ

```powershell
docker compose up -d redis rabbitmq
```

如果出现下面的错误：

```text
proxyconnect tcp: dial tcp 127.0.0.1:7892: connect: connection refused
```

说明 Docker Desktop 配置了代理 `127.0.0.1:7892`，但该代理端口没有启动。可任选一种方式处理：

1. 打开你的代理软件，确认 HTTP 代理端口是 `7892`，然后重新执行 `docker compose up -d redis rabbitmq`。
2. 在 Docker Desktop 中进入 `Settings -> Resources -> Proxies`，关闭代理或改成当前真实代理端口，点击 `Apply & restart` 后重试。
3. 不使用代理时，在 Docker Desktop 关闭代理配置；如果仍失败，重启 Docker Desktop。

Redis 和 RabbitMQ 启动成功后可检查：

```powershell
docker ps
```

如果出现下面的超时：

```text
net/http: request canceled while waiting for connection
Client.Timeout exceeded while awaiting headers
```

说明 Docker 已经尝试访问 Docker Hub，但连接 Docker Hub 超时。可按顺序尝试：

1. 在 Docker Desktop 中配置可用代理或镜像加速器，然后 `Apply & restart`。
2. 先单独拉取镜像，确认网络是否恢复：

```powershell
docker pull redis:7-alpine
docker pull rabbitmq:3-management
```

3. 如果 Docker Hub 一直不可用，可以改用本机 Redis 和 RabbitMQ。只要 Redis 监听 `127.0.0.1:6379`、RabbitMQ 监听 `127.0.0.1:5672`，项目无需修改 `.env`。

当前实现还带有课程演示兜底：如果 cloud-api 或 worker 启动时连接 RabbitMQ 超时，系统会自动降级为 MySQL 轮询 `queued` 图片任务，保证端到端演示可继续跑通。RabbitMQ 可用时优先走 `image.process` 队列；Redis 只用于查询缓存。

## 4. 启动 Go 服务

推荐一键启动全部服务：

```powershell
.\start-all.cmd
```

本机手动部署压测时可以使用：

```powershell
.\start-all-pressure.cmd
```

它会以 `EDGE_DEDUP_ENABLED=false` 启动 edge-node，并把默认模拟器间隔调成 1 秒。

它会自动完成：

```text
1. 编译 Go 后端
2. 启动 cloud-api
3. 启动 worker
4. 启动 edge-node
5. 启动 Vue 前端
6. 启动 device_001 模拟器
```

停止项目可运行：

```powershell
.\stop-all.cmd
```

注意：`stop-all.cmd` 会关闭当前系统中的 `node.exe` 和 `python.exe`，如果你同时运行了其他 Node/Python 程序，请手动关闭本项目窗口。

也可以手动启动：

在三个 PowerShell 窗口分别运行：

```powershell
go run ./cmd/cloud-api
```

```powershell
go run ./cmd/worker
```

```powershell
go run ./cmd/edge-node
```

服务地址：

```text
cloud-api: http://127.0.0.1:8080
edge-node: http://127.0.0.1:8081
```

## 5. 启动设备模拟器

```powershell
conda create -n iot-vision python=3.12 -y
conda activate iot-vision
pip install -r device-simulator/requirements.txt
python device-simulator/simulator.py --device-id device_001 --interval 3
```

默认使用 HTTP 协议并走完整链路上传到 edge-node：

```text
http://127.0.0.1:8081/api/edge/upload
```

如果要绕过 edge-node，直接通过 nginx 网关上传到 cloud-api：

```powershell
python device-simulator/simulator.py --route gateway --once
```

如果要通过 MQTT 上传，先确保 Docker 中的 `mqtt-broker` 和 `edge-node` 已启动，然后运行：

```powershell
python device-simulator/simulator.py --protocol mqtt --once
```

MQTT 模式会发布到 `iot/images/{device_id}`，payload 是 JSON + base64 单消息。

也可以完全手动指定上传地址：

```powershell
python device-simulator/simulator.py --upload-url http://127.0.0.1:5173/api/images/upload --once
```

可再开两个窗口模拟多设备：

```powershell
python device-simulator/simulator.py --device-id device_002 --interval 4
python device-simulator/simulator.py --device-id device_003 --interval 5
```

模拟器首次运行会自动生成 `device-simulator/sample-images/` 测试图片。

## 6. 启动前端

PowerShell 执行策略可能拦截 `npm.ps1`，请使用 `npm.cmd`：

```powershell
cd frontend
npm.cmd install --registry=https://registry.npmmirror.com
npm.cmd run dev
```

打开：

```text
http://127.0.0.1:5173
```

## API 摘要

### 用户登录

```http
POST /api/auth/login
Content-Type: application/json
```

请求：

```json
{
  "username": "admin",
  "password": "admin123456"
}
```

返回：

```json
{
  "token": "...",
  "user": {
    "id": 1,
    "username": "admin",
    "role": "admin"
  }
}
```

### 当前用户

```http
GET /api/auth/me
Authorization: Bearer <token>
```

### 上传图片

```http
POST /api/images/upload
X-Device-Token: course-demo-token
Content-Type: multipart/form-data
```

字段：

```text
file
device_id
edge_node_id
captured_at
```

### 查询图片

```http
GET /api/images?device_id=device_001&status=completed&tag=wide-view&page=1&page_size=60&sort_by=created_at&sort_order=desc
Authorization: Bearer <token>
```

支持参数：`device_id`、`tag`、`status`、`start_time`、`end_time`、`page`、`page_size`、`sort_by`、`sort_order`。
`page_size` 最大 200；`sort_by` 支持 `created_at`、`captured_at`、`updated_at`、`size`、`device_id`；`sort_order` 支持 `desc` 和 `asc`。

### 查询设备

```http
GET /api/devices
Authorization: Bearer <token>
```

### 查询任务状态

```http
GET /api/tasks/status
Authorization: Bearer <token>
```

### 查询统计

```http
GET /api/stats
Authorization: Bearer <token>
```

公开接口：

```text
GET  /api/health
POST /api/auth/login
POST /api/images/upload
GET  /files/original/...
GET  /files/thumbnail/...
```

需要 JWT 的接口：

```text
GET /api/auth/me
GET /api/images
GET /api/images/:image_id
GET /api/devices
GET /api/tasks/status
GET /api/stats
```

## gRPC AI 与目标检测服务

Worker 的 AI 通信使用同一套 gRPC 协议：

```text
worker -> /vision.v1.VisionAnalysisService/AnalyzeImage
```

协议文件：

```text
proto/vision/v1/vision.proto
```

Docker 内的 `ai-service` 是规则分类服务。当前 worker 默认不再依赖它，标签改为 Go 本地规则生成，保证链路简单稳定。

新增的 `ai-detection-service` 是独立本机 GPU 服务，不放进 Docker。它使用 ModelScope 模型 `iic/cv_tinynas_head-detection_damoyolo` 做目标检测，返回 proto 中已有的 `detection.detections`。worker 会把检测框画到缩略图上，并继续写入原来的 `thumbnail/<image_id>.jpg`，前端无需修改。

创建干净 conda 环境：

```powershell
cd ai-detection-service
conda env create -f environment.yml
cd ..
```

启动本机 GPU detection 服务：

```powershell
conda activate iot-detection-gpu
python ai-detection-service\server.py
```

worker 连接 detection 服务的配置：

```text
DETECTION_RPC_ENABLED=true
DETECTION_RPC_ADDR=host.docker.internal:9100
DETECTION_RPC_TIMEOUT_SECONDS=10
PUBLIC_GATEWAY_URL=http://127.0.0.1:5173
```

`PUBLIC_GATEWAY_URL` 是外部服务可访问的平台统一入口，用于 LOCAL 存储模式：worker 传给本机 detection 服务的图片地址会变成 `http://127.0.0.1:5173/files/original/...`，这样宿主机服务可以通过 nginx 网关读取容器内业务图片。HUAWEI 模式下，worker 仍优先传 OBS public URL 或 presigned URL。旧变量 `DETECTION_IMAGE_BASE_URL` 仍作为兼容 fallback 保留。

如果 detection 服务不可用、超时或没有检测框，worker 会降级生成普通缩略图，图片任务仍会完成。

VOC2028 冒烟测试：

```powershell
conda activate iot-detection-gpu
python ai-detection-service\test_detection_voc2028.py --limit 10
```

脚本默认读取 `VOC2028/VOC2028/JPEGImages`，输出带红框图片到 `ai-detection-service/test-output/`。

重新生成 Go/Python Protobuf 代码：

```powershell
.\generate-proto.cmd
```

默认使用：

```text
D:\tools\protoc-35.0-rc-2-win64\bin\protoc.exe
```

如果 protoc 在其他路径，可先设置：

```powershell
$env:PROTOC_PATH="D:\tools\protoc-35.0-rc-2-win64\bin\protoc.exe"
.\generate-proto.cmd
```

Worker 支持横向扩展：

```powershell
docker compose up -d --scale worker=3 worker
```

本机 detection 服务默认是单实例 GPU 推理服务；如果要多个实例，需要使用不同端口启动并在 worker 前面增加负载均衡。

## Storage 抽象层

后端已经提供对象存储抽象接口，代码位于：

```text
internal/platform/object_storage.go
internal/platform/local_storage.go
internal/platform/huawei_storage.go
```

统一接口：

```go
type Storage interface {
    Provider() StorageProvider
    Bucket() string
    Put(ctx context.Context, key string, body io.Reader, opts PutObjectOptions) (StoredObject, error)
    Get(ctx context.Context, key string) (io.ReadCloser, StoredObject, error)
    Delete(ctx context.Context, key string) error
    PresignGet(ctx context.Context, key string, expires time.Duration) (string, error)
}
```

当前实现：

```text
LocalStorage   本地文件系统实现，适合课程演示和 Docker volume
HuaweiStorage  华为云 OBS 实现，适合云端对象存储
```

当前 `STORAGE_PROVIDER` 支持：

```text
LOCAL   业务图片保存到本地 storage/original 与 storage/thumbnail
HUAWEI  业务原图和缩略图保存到华为 OBS，worker 与 ai-service 从 OBS 读取原图
```

`STORAGE_PROVIDER` 是业务图片通道的统一开关。edge-node 的失败重传缓存和去重记录仍保留在本地 `storage/edge-cache`，用于维持边缘节点的离线缓存能力。

Huawei OBS 环境变量示例：

```text
HUAWEI_OBS_ACCESS_KEY=你的AK
HUAWEI_OBS_SECRET_KEY=你的SK
HUAWEI_OBS_SECURITY_TOKEN=临时凭证时填写
HUAWEI_OBS_ENDPOINT=https://obs.cn-north-4.myhuaweicloud.com
HUAWEI_OBS_BUCKET=你的bucket
HUAWEI_OBS_PUBLIC_URL=https://你的bucket.obs.cn-north-4.myhuaweicloud.com
```

启用华为 OBS 作为业务图片存储：

```powershell
$env:STORAGE_PROVIDER="HUAWEI"
$env:STORAGE_OBJECT_PREFIX="original"
docker compose up --build -d cloud-api worker frontend
```

上传成功后，MySQL `images` 表会保存当前存储中的 `original_path`。在 OBS 模式下，它是对象 key，同时还会写入：

```text
original_storage_provider
original_bucket
original_object_key
original_object_url
original_storage_error
```

启用后，cloud-api 会把上传原图写入 OBS，worker 从 OBS 拉取原图并把缩略图写回 OBS；前端图片 URL 优先使用公开 URL，未配置 `HUAWEI_OBS_PUBLIC_URL` 时由后端生成临时签名 URL。

## 课程要求对应关系

| 课程要求 | 项目体现 |
| --- | --- |
| 传感层 | Python 设备模拟器模拟摄像头采集 |
| 网络层 | HTTP multipart 上传、设备 token 校验 |
| 应用层 | Vue 图库、设备、任务和统计页面 |
| 云边端协同 | 端侧采集、边缘去重缓存、云端存储分析 |
| 流处理 | RabbitMQ `image.process` 队列 |
| 一般处理 | 图片查询、统计、缩略图生成 |
| 分布式计算 | Worker 使用 RabbitMQ manual ack + prefetch，可启动多个并行消费 |
| 智能分析 | 基于主色、尺寸、文件名的轻量 AI 标签 |
| 网络安全 | token 校验、文件类型限制、文件大小限制 |

## 演示流程

1. MySQL 执行 `deploy/mysql/init.sql`。
2. `docker compose up -d redis rabbitmq`。
3. 启动 `cloud-api`、`worker`、`edge-node`。
4. 启动 3 个 `device-simulator`。
5. 打开 Vue 前端，观察图库新增图片、任务状态从 `queued` 变为 `completed`，设备列表更新。

## 常见问题

- `mysql` 命令不可用：使用 MySQL Workbench 等工具执行 SQL，或把 MySQL bin 目录加入 PATH。
- `redis-server` 不可用：本项目默认用 Docker 启动 Redis。
- `npm.ps1 cannot be loaded`：使用 `npm.cmd install` 和 `npm.cmd run dev`。
- 上传后一直 `queued`：确认 Worker 正在运行，RabbitMQ 地址和 `.env` 一致。
- 图片上传失败：确认 `X-Device-Token` 与 `.env` 中的 `DEVICE_TOKEN` 一致。
