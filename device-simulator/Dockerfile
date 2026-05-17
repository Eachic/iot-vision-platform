ARG IMAGE_REGISTRY=docker.m.daocloud.io/library

FROM ${IMAGE_REGISTRY}/python:3.12-slim

WORKDIR /app
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt -i https://pypi.tuna.tsinghua.edu.cn/simple

COPY simulator.py ./simulator.py

CMD ["python", "simulator.py", "--device-id", "device_docker_001", "--edge-url", "http://edge-node:8081/api/edge/upload", "--interval", "3"]
