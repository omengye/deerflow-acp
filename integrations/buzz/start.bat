@echo off
setlocal DisableDelayedExpansion

cd /d "%~dp0"

rem Load only the adapter's supported secrets from a local .env file.
rem Values already present in the process environment take precedence.
if exist ".env" (
    for /f "usebackq eol=# tokens=1,* delims==" %%A in (".env") do (
        if /I "%%A"=="BUZZ_PRIVATE_KEY" if not defined BUZZ_PRIVATE_KEY set "BUZZ_PRIVATE_KEY=%%~B"
        if /I "%%A"=="BUZZ_AUTH_TAG" if not defined BUZZ_AUTH_TAG set "BUZZ_AUTH_TAG=%%~B"
    )
)

if not exist "adapter.toml" (
    echo [ERROR] adapter.toml was not found in:
    echo %CD%
    echo Copy adapter.example.toml to adapter.toml and configure it first.
    pause
    exit /b 1
)

if "%BUZZ_PRIVATE_KEY%"=="" (
    echo [ERROR] BUZZ_PRIVATE_KEY is not set in this process environment.
    echo Set it in the process environment or in .env next to start.bat.
    echo Never place the secret in adapter.toml.
    pause
    exit /b 1
)

echo Starting Buzz-DeerFlow adapter...
if exist ".venv\Scripts\python.exe" (
    ".venv\Scripts\python.exe" -m buzz_deerflow_adapter --config ".\adapter.toml" %*
) else (
    where uv >nul 2>&1
    if errorlevel 1 (
        echo [ERROR] Neither .venv\Scripts\python.exe nor uv was found.
        echo Run uv sync in this folder, then try again.
        pause
        exit /b 1
    )
    uv run buzz-deerflow-adapter --config ".\adapter.toml" %*
)
set "ADAPTER_EXIT_CODE=%ERRORLEVEL%"

if not "%ADAPTER_EXIT_CODE%"=="0" (
    echo.
    echo [ERROR] Adapter exited with code %ADAPTER_EXIT_CODE%.
    pause
)

exit /b %ADAPTER_EXIT_CODE%
