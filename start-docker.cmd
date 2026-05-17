@echo off
setlocal

cd /d "%~dp0"

echo Starting IoT Vision Platform with Docker Compose...
docker compose up --build -d
if errorlevel 1 goto fail

echo.
echo Core services are starting. Check status with:
echo   docker compose ps
echo.
echo Frontend:
echo   http://127.0.0.1:5173
echo Cloud API:
echo   http://127.0.0.1:8080/api/health
echo.
echo To start one simulator container:
echo   docker compose --profile simulator up --build -d simulator
goto end

:fail
echo.
echo Docker startup failed. Check Docker Desktop, proxy, and image pull network.
pause

:end
endlocal
