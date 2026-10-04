@echo off
REM Start the Eino Go ACP daemon through the native Bridge.
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0acp-daemon.ps1" -Action start %*
exit /b %ERRORLEVEL%
