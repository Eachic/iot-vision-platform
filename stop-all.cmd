@echo off
echo Stopping IoT vision platform processes...
taskkill /F /IM cloud-api.exe >nul 2>nul
taskkill /F /IM worker.exe >nul 2>nul
taskkill /F /IM edge-node.exe >nul 2>nul
taskkill /F /IM node.exe >nul 2>nul
taskkill /F /IM python.exe >nul 2>nul
echo Done.
pause
