ARG IMAGE_REGISTRY=docker.m.daocloud.io/library

FROM ${IMAGE_REGISTRY}/python:3.12-slim

WORKDIR /app
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt -i https://pypi.tuna.tsinghua.edu.cn/simple

COPY proto ./proto
COPY server.py ./server.py

EXPOSE 9000
CMD ["python", "server.py"]
