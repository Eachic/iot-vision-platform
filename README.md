# 基于云边端协同的物联网视觉数据平台

这是一个用于《物联网技术与应用》课程报告的 MVP 项目，覆盖端侧采集、边缘去重、云端存储、Redis Stream 流处理、Worker 异步图片处理、MySQL 元数据管理和 Vue 可视化展示。

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
python pressure-test/pressure_upload.py --low
python pressure-test/pressure_upload.py --mid
python pressure-test/pressure_upload.py --high
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
python pressure-test/pressure_upload.py --low --target http://127.0.0.1:8080/api/images/upload
```

默认 Dockerfile 使用 `docker.m.daocloud.io/library` 作为基础镜像源，避免直接访问 Docker Hub 超时。如果你的网络可以直连 Docker Hub，也可以把 `docker-compose.yml` 中的 `IMAGE_REGISTRY` 改成 `docker.io/library`。

等容器启动后打开：

```text
http://127.0.0.1:5173
```

查看服务状态：

```powershell
docker compose ps
```

查看日志：

```powershell
docker compose logs -f cloud-api
docker compose logs -f worker
docker compose logs -f edge-node
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
frontend:  http://127.0.0.1:5173
cloud-api: http://127.0.0.1:8080
edge-node: http://127.0.0.1:8081
mysql:     127.0.0.1:3307
redis:     127.0.0.1:6379
```

注意：Docker 内部服务仍通过 `mysql:3306` 通信；为了避免和本机 MySQL 冲突，默认把容器 MySQL 映射到宿主机 `3307`。

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
DEVICE_TOKEN=course-demo-token
EDGE_DEDUP_ENABLED=true
CLOUD_API_ADDR=:8080
EDGE_NODE_ADDR=:8081
CLOUD_UPLOAD_URL=http://127.0.0.1:8080/api/images/upload
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
```

支持参数：`device_id`、`tag`、`status`、`start_time`、`end_time`、`page`、`page_size`、`sort_by`、`sort_order`。
`page_size` 最大 200；`sort_by` 支持 `created_at`、`captured_at`、`updated_at`、`size`、`device_id`；`sort_order` 支持 `desc` 和 `asc`。

### 查询设备

```http
GET /api/devices
```

### 查询任务状态

```http
GET /api/tasks/status
```

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

Huawei OBS 环境变量示例：

```text
HUAWEI_OBS_ACCESS_KEY=你的AK
HUAWEI_OBS_SECRET_KEY=你的SK
HUAWEI_OBS_SECURITY_TOKEN=临时凭证时填写
HUAWEI_OBS_ENDPOINT=https://obs.cn-north-4.myhuaweicloud.com
HUAWEI_OBS_BUCKET=你的bucket
HUAWEI_OBS_PUBLIC_URL=https://你的bucket.obs.cn-north-4.myhuaweicloud.com
```

目前业务主链路仍使用本地 `storage/original` 和 `storage/thumbnail` 字段。下一步迁移到 OSS 时，只需要把 `cloud-api` 保存原图、`worker` 读取原图和保存缩略图的地方切换为 `Storage` 接口，并在 MySQL 中增加 `provider/bucket/key` 字段。

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
