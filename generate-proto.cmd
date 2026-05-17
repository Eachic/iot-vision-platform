@echo off
setlocal

cd /d "%~dp0"

if "%PROTOC_PATH%"=="" (
  set "PROTOC_PATH=D:\tools\protoc-35.0-rc-2-win64\bin\protoc.exe"
)

if not exist "%PROTOC_PATH%" (
  echo protoc not found: %PROTOC_PATH%
  echo Set PROTOC_PATH to your protoc.exe path.
  exit /b 1
)

set "GOPROXY=https://goproxy.cn,direct"
set "GOBIN=%CD%\.tools\bin"
set "GOMODCACHE=%CD%\.gocache\mod"
set "GOCACHE=%CD%\.gocache\build"
if not exist "%GOBIN%" mkdir "%GOBIN%"

echo Installing Go protoc plugins into .tools\bin...
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
if errorlevel 1 exit /b 1
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
if errorlevel 1 exit /b 1

set "PATH=%GOBIN%;%PATH%"

echo Generating Go protobuf code...
"%PROTOC_PATH%" --proto_path=. --go_out=. --go_opt=module=iot-vision-platform --go-grpc_out=. --go-grpc_opt=module=iot-vision-platform proto\vision\v1\vision.proto
if errorlevel 1 exit /b 1

echo Generating Python protobuf messages...
"%PROTOC_PATH%" --proto_path=. --python_out=ai-service proto\vision\v1\vision.proto
if errorlevel 1 exit /b 1

echo Relaxing Python protobuf runtime check for local Docker compatibility...
powershell -NoProfile -Command "$p='ai-service\proto\vision\v1\vision_pb2.py'; $t=Get-Content -Raw -Path $p; $t=$t -replace 'from google\.protobuf import runtime_version as _runtime_version\r?\n',''; $t=$t -replace '_runtime_version\.ValidateProtobufRuntimeVersion\([\s\S]*?\)\r?\n',''; Set-Content -Path $p -Value $t -NoNewline -Encoding utf8"
if errorlevel 1 exit /b 1

echo Done.
endlocal
