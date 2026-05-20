@echo off
setlocal

cd /d "%~dp0"

echo Starting IoT Vision Platform pressure-test mode...
echo Edge duplicate filtering will be disabled.
set "EDGE_DEDUP_ENABLED=false"

docker compose up --build -d
if errorlevel 1 goto fail

echo.
echo Pressure-test mode is starting. Check edge status with:
echo   curl http://127.0.0.1:8081/api/edge/status
echo.
echo Expected edge status field:
echo   "dedup_enabled": false
echo.
echo Frontend:
echo   http://127.0.0.1:5173
echo API Gateway health:
echo   http://127.0.0.1:5173/api/health
echo.
echo To generate more upload traffic:
echo   docker compose --profile simulator up --build -d --scale simulator=3 simulator
goto end

:fail
echo.
echo Docker pressure-test startup failed. Check Docker Desktop and compose logs.
pause

:end
endlocal
