@echo off
setlocal
cd /d "%~dp0"
python pressure-test\pressure_upload.py --mid --route gateway
endlocal
