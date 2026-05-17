ARG IMAGE_REGISTRY=docker.m.daocloud.io/library

FROM ${IMAGE_REGISTRY}/node:22-alpine AS builder

WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --registry=https://registry.npmmirror.com

COPY . .
RUN npm run build

FROM ${IMAGE_REGISTRY}/nginx:1.27-alpine

COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=builder /app/dist /usr/share/nginx/html

EXPOSE 80
