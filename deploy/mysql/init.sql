CREATE DATABASE IF NOT EXISTS iot_vision
  DEFAULT CHARACTER SET utf8mb4
  DEFAULT COLLATE utf8mb4_unicode_ci;

CREATE USER IF NOT EXISTS 'iot_user'@'localhost' IDENTIFIED BY 'iot_password';
CREATE USER IF NOT EXISTS 'iot_user'@'%' IDENTIFIED BY 'iot_password';

GRANT ALL PRIVILEGES ON iot_vision.* TO 'iot_user'@'localhost';
GRANT ALL PRIVILEGES ON iot_vision.* TO 'iot_user'@'%';
FLUSH PRIVILEGES;

USE iot_vision;

CREATE TABLE IF NOT EXISTS devices (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  device_id VARCHAR(64) NOT NULL UNIQUE,
  name VARCHAR(128) NOT NULL,
  location VARCHAR(128) NOT NULL,
  status VARCHAR(32) NOT NULL,
  last_seen DATETIME(3) NULL,
  created_at DATETIME(3) NULL,
  updated_at DATETIME(3) NULL,
  INDEX idx_devices_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS images (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  image_id VARCHAR(64) NOT NULL UNIQUE,
  device_id VARCHAR(64) NOT NULL,
  edge_node_id VARCHAR(64) NOT NULL,
  original_path VARCHAR(512) NOT NULL,
  thumbnail_path VARCHAR(512) NOT NULL,
  original_storage_provider VARCHAR(32) NULL,
  original_bucket VARCHAR(128) NULL,
  original_object_key VARCHAR(512) NULL,
  original_object_url VARCHAR(1024) NULL,
  original_storage_error TEXT NULL,
  hash VARCHAR(128) NOT NULL,
  width INT NOT NULL DEFAULT 0,
  height INT NOT NULL DEFAULT 0,
  size BIGINT NOT NULL DEFAULT 0,
  format VARCHAR(32) NOT NULL,
  status VARCHAR(32) NOT NULL,
  error_message TEXT NULL,
  captured_at DATETIME(3) NULL,
  created_at DATETIME(3) NULL,
  updated_at DATETIME(3) NULL,
  INDEX idx_images_device_id (device_id),
  INDEX idx_images_status (status),
  INDEX idx_images_hash (hash),
  INDEX idx_images_captured_at (captured_at),
  INDEX idx_images_created_at (created_at),
  INDEX idx_images_status_created_at (status, created_at),
  INDEX idx_images_device_created_at (device_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS image_tags (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  image_id VARCHAR(64) NOT NULL,
  tag VARCHAR(64) NOT NULL,
  confidence DOUBLE NOT NULL DEFAULT 0,
  created_at DATETIME(3) NULL,
  INDEX idx_image_tags_image_id (image_id),
  INDEX idx_image_tags_tag (tag)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS users (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  username VARCHAR(64) NOT NULL UNIQUE,
  password_hash VARCHAR(128) NOT NULL,
  role VARCHAR(32) NOT NULL,
  created_at DATETIME(3) NULL,
  updated_at DATETIME(3) NULL,
  INDEX idx_users_role (role)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
