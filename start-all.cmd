@echo off
setlocal

set "ROOT=%~dp0"
cd /d "%ROOT%"

if not exist ".env" (
  echo Creating .env from .env.example
  copy ".env.example" ".env" >nul
)

if not exist "logs" mkdir "logs"
if not exist "storage" mkdir "storage"

echo Building Go services...
set "GOPROXY=https://goproxy.cn,direct"
if not exist "bin" mkdir "bin"
go build -o "bin\cloud-api.exe" ./cmd/cloud-api
if errorlevel 1 goto fail
go build -o "bin\worker.exe" ./cmd/worker
if errorlevel 1 goto fail
go build -o "bin\edge-node.exe" ./cmd/edge-node
if errorlevel 1 goto fail

echo Starting backend services...
start "IoT Cloud API :8080" cmd /k "cd /d ""%ROOT%"" && bin\cloud-api.exe"
timeout /t 2 /nobreak >nul
start "IoT Worker" cmd /k "cd /d ""%ROOT%"" && bin\worker.exe"
timeout /t 1 /nobreak >nul
start "IoT Edge Node :8081" cmd /k "cd /d ""%ROOT%"" && bin\edge-node.exe"
timeout /t 2 /nobreak >nul

echo Preparing frontend...
cd /d "%ROOT%frontend"
if not exist "node_modules" (
  npm.cmd install --registry=https://registry.npmmirror.com
  if errorlevel 1 goto fail
)

echo Starting frontend :5173...
start "IoT Frontend :5173" cmd /k "cd /d ""%ROOT%frontend"" && npm.cmd run dev"

echo Starting default device simulator...
cd /d "%ROOT%"
start "IoT Device device_001" cmd /k "cd /d ""%ROOT%"" && python device-simulator\simulator.py --device-id device_001 --interval 3"

echo.
echo All services are starting.
echo Frontend: http://127.0.0.1:5173
echo Cloud API: http://127.0.0.1:8080
echo Edge Node: http://127.0.0.1:8081
echo.
echo Keep the opened windows running. Close those windows to stop services.
goto end

:fail
echo.
echo Startup failed. Check the error above.
pause

:end
endlocal
