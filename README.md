# 基于云边端协同的物联网视觉数据平台

这是一个用于《物联网技术与应用》课程报告的 MVP 项目，覆盖端侧采集、边缘去重、云端存储、Redis Stream 流处理、Redis 短 TTL 查询缓存、Worker 异步图片处理、MySQL 元数据管理和 Vue 可视化展示。

## 目录结构

```text
iot-vision-platform/
├── cmd/
│   ├── cloud-api/      # 云端 API 服务
│   ├── edge-node/      # 边缘节点服务
│   └── worker/         # Redis Stream 图片处理 Worker
├── device-simulator/   # Python 设备模拟器
├── frontend/           # Vue3 前端
├── deploy/mysql/       # MySQL 初始化 SQL
├── internal/platform/  # Go 共享配置、模型、图片处理工具
├── storage/            # 运行时图片和边缘缓存目录
├── docker-compose.yml  # 默认只启动 Redis
└── .env.example
```

## 数据流说明

平台有两条核心数据流：一条是端侧设备上传图片并触发云端异步处理，另一条是用户通过 Web 前端查看图片和元数据。`STORAGE_PROVIDER` 控制业务图片实际存储位置：`LOCAL` 时使用本地 `storage` 目录，`HUAWEI` 时使用华为云 OBS。edge-node 的失败重传缓存始终保留在本地 `storage/edge-cache`，用于保证边缘节点在云端短暂不可用时仍能接收设备数据。

### 端侧设备上传图片

```text
device-simulator / pressure-test
  -> edge-node :8081 /api/edge/upload
  -> nginx gateway(frontend :5173) /api/images/upload
  -> cloud-api :8080
  -> Storage(LOCAL 或 HUAWEI OBS)
  -> MySQL images/devices
  -> Redis Stream image_uploaded_stream
  -> worker
  -> Storage 读取原图、写入缩略图
  -> ai-service 读取原图并生成标签
  -> MySQL image_tags/images 状态更新
```

1. 端侧设备通过 HTTP multipart 上传图片到 `edge-node`，请求需要携带 `X-Device-Token`，并提交 `device_id`、`edge_node_id`、`captured_at` 等表单字段。
2. `edge-node` 校验设备 token，计算图片 SHA256。启用 `EDGE_DEDUP_ENABLED=true` 时，重复图片会在边缘侧直接过滤，减少云端压力。
3. `edge-node` 把图片转发到 `cloud-api`。如果云端暂时不可用，图片和元数据会写入本地 `storage/edge-cache`，后台重试循环会定时重新转发。
4. `cloud-api` 再次校验设备 token，并通过统一 `Storage` 接口保存原图：
   - `STORAGE_PROVIDER=LOCAL`：写入本地 `storage/original/...`。
   - `STORAGE_PROVIDER=HUAWEI`：写入华为 OBS 的 `original/...` 对象。
5. `cloud-api` 在 MySQL 中创建图片记录，状态为 `queued`，同时更新或创建设备记录。`images.original_path` 保存当前存储中的 key，OBS 模式下还会写入 `original_storage_provider`、`original_bucket`、`original_object_key`、`original_object_url` 等字段。
6. `cloud-api` 向 Redis Stream `image_uploaded_stream` 写入任务消息，消息中包含 `image_id`、`original_path`、`device_id`、`edge_node_id` 等信息。如果 Redis 不可用，worker 会降级轮询 MySQL 中的 `queued` 任务。
7. `worker` 消费任务后把图片状态改为 `processing`，通过统一 `Storage.Get` 读取原图，不再直接依赖本地文件路径。
8. `worker` 解码图片、计算 hash、读取尺寸和格式，并生成 JPEG 缩略图。缩略图通过统一 `Storage.Put` 写入：
   - `LOCAL`：本地 `storage/thumbnail/...`。
   - `HUAWEI`：OBS 的 `thumbnail/...` 对象。
9. `worker` 调用 `ai-service` 做标签分析。`LOCAL` 模式下传本地共享 volume 路径；`HUAWEI` 模式下优先传 OBS public URL，未配置公开 URL 时传临时签名 URL，同时在 gRPC `params` 中传递 `storage_provider` 和 `storage_key`。
10. `ai-service` 读取图片，按当前规则分类逻辑生成标签。调用失败或超时时，worker 会降级使用 Go 本地规则标签，保证图片任务仍能完成。
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

### Redis 缓存层

Redis 在当前项目里承担两类职责：

- 消息队列：`cloud-api` 上传成功后向 Redis Stream `image_uploaded_stream` 写入图片处理任务，worker 通过 consumer group 并行消费。
- 查询缓存：`cloud-api` 对高频聚合接口使用短 TTL JSON 缓存，降低前端轮询时对 MySQL 的重复查询压力。

当前缓存范围：

| 接口 | Redis key | TTL |
| --- | --- | --- |
| `GET /api/stats` | `cache:cloud-api:stats` | 5 秒 |
| `GET /api/tasks/status` | `cache:cloud-api:tasks_status` | 2 秒 |
| `GET /api/devices` | `cache:cloud-api:devices` | 10 秒 |

暂时不缓存 `GET /api/images`，因为图片列表存在分页、筛选、排序和对象存储签名 URL，缓存失效和 URL 过期处理更复杂。

缓存一致性采用“短 TTL + 上传后主动失效”的方式：`cloud-api` 每次成功接收新图片后会删除以上三个缓存 key，让新增图片和设备尽快体现在前端；worker 更新任务状态和标签时不主动操作 cloud-api 缓存，依赖 `/api/tasks/status` 的 2 秒 TTL 快速收敛。Redis 不可用时，接口会自动回退到 MySQL 查询，核心上传和展示链路仍可运行。

`/api/tasks/status` 还会顺带清理异常中断留下的孤儿任务：如果某张图片保持 `processing` 超过 10 分钟，说明 worker 很可能在处理过程中被停止或重启，`cloud-api` 会把这类记录标记为 `failed` 并写入超时错误信息，避免前端任务状态长期卡在“处理中”。

多个 worker 高并发写入 MySQL 时，`image_tags` 删除、插入和 `images` 状态更新可能遇到 MySQL 死锁 `Error 1213 (40001)` 或锁等待超时。worker 会对这类可重试数据库错误自动重试事务；如果数据库暂时不可写，Redis Stream 消息不会被 ACK，会留在 pending 列表中，空闲 5 分钟后由 worker 重新认领。这个时间要明显长于正常图片处理耗时，避免一张正在处理的图片被其他 worker 过早重复认领。

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

启动一个容器内设备模拟器：

```powershell
docker compose --profile simulator up --build -d simulator
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
mysql:                  127.0.0.1:3307
redis:                  127.0.0.1:6379
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

## 3. 启动 Redis

```powershell
docker compose up -d redis
```

如果出现下面的错误：

```text
proxyconnect tcp: dial tcp 127.0.0.1:7892: connect: connection refused
```

说明 Docker Desktop 配置了代理 `127.0.0.1:7892`，但该代理端口没有启动。可任选一种方式处理：

1. 打开你的代理软件，确认 HTTP 代理端口是 `7892`，然后重新执行 `docker compose up -d redis`。
2. 在 Docker Desktop 中进入 `Settings -> Resources -> Proxies`，关闭代理或改成当前真实代理端口，点击 `Apply & restart` 后重试。
3. 不使用代理时，在 Docker Desktop 关闭代理配置；如果仍失败，重启 Docker Desktop。

Redis 启动成功后可检查：

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
```

3. 如果 Docker Hub 一直不可用，可以改用本机 Redis。只要有 Redis 服务监听 `127.0.0.1:6379`，项目无需修改 `.env`。

当前实现还带有课程演示兜底：如果云端 API 或 Worker 连接 Redis 超时，系统会自动降级为 MySQL 轮询 `queued` 图片任务，保证端到端演示可继续跑通。Redis 可用时仍优先走 Redis Stream。

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

## gRPC AI 服务

Worker 已经把标签生成重构为可选 gRPC 调用：

```text
worker -> ai-service:9000 -> /vision.v1.VisionAnalysisService/AnalyzeImage
```

第一版 `ai-service` 是 Python 服务，暂不接真实模型，只复刻原有规则标签逻辑。gRPC 请求只传图片 URI，不传图片 bytes：

```text
image_uri=/app/storage/original/img_xxx.jpg
```

相关配置：

```text
AI_RPC_ENABLED=true
AI_RPC_ADDR=ai-service:9000
AI_RPC_TIMEOUT_SECONDS=5
```

如果 `ai-service` 不可用、超时或无法读取图片，Worker 会自动降级为 Go 本地规则标签，图片任务仍会完成。

协议文件：

```text
proto/vision/v1/vision.proto
```

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

Worker 和 AI 服务均支持横向扩展：

```powershell
docker compose up -d --scale worker=3 worker
docker compose up -d --scale ai-service=3 ai-service
```

`worker` 与 `ai-service` 都没有固定 `container_name`，多个实例不会发生容器命名冲突。

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
| 流处理 | Redis Stream `image_uploaded_stream` |
| 一般处理 | 图片查询、统计、缩略图生成 |
| 分布式计算 | Worker 使用 consumer group，可启动多个并行消费 |
| 智能分析 | 基于主色、尺寸、文件名的轻量 AI 标签 |
| 网络安全 | token 校验、文件类型限制、文件大小限制 |

## 演示流程

1. MySQL 执行 `deploy/mysql/init.sql`。
2. `docker compose up -d redis`。
3. 启动 `cloud-api`、`worker`、`edge-node`。
4. 启动 3 个 `device-simulator`。
5. 打开 Vue 前端，观察图库新增图片、任务状态从 `queued` 变为 `completed`，设备列表更新。

## 常见问题

- `mysql` 命令不可用：使用 MySQL Workbench 等工具执行 SQL，或把 MySQL bin 目录加入 PATH。
- `redis-server` 不可用：本项目默认用 Docker 启动 Redis。
- `npm.ps1 cannot be loaded`：使用 `npm.cmd install` 和 `npm.cmd run dev`。
- 上传后一直 `queued`：确认 Worker 正在运行，Redis 地址和 `.env` 一致。
- 图片上传失败：确认 `X-Device-Token` 与 `.env` 中的 `DEVICE_TOKEN` 一致。
