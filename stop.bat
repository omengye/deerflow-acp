@echo off
REM Stop the selected Eino Go ACP daemon through authenticated IPC.
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0stop.ps1" %*
exit /b %ERRORLEVEL%
