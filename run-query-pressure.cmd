@echo off
setlocal
cd /d "%~dp0"
powershell -NoProfile -ExecutionPolicy Bypass -File pressure-test\query_hey.ps1 %*
endlocal
