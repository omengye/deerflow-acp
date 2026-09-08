[CmdletBinding()]
param(
    [ValidateSet("Debug", "Release")]
    [string]$Configuration = "Release",
    [string]$PythonVersion = "3.12.10",
    [string]$OutputDirectory = "dist\portable\DeerFlow",
    [switch]$SkipZip,
    # Also build the client-facing ACP agent archive (deerflow-acp.zip) in
    # dist\portable next to the portable zip. Skipped when -SkipZip is set.
    [switch]$SkipAcpZip
)

$ErrorActionPreference = "Stop"

function Get-CachedRemoteFile {
    param(
        [Parameter(Mandatory = $true)][string[]]$Urls,
        [Parameter(Mandatory = $true)][string]$Destination
    )
    if (Test-Path -LiteralPath $Destination) { return }
    $lastError = $null
    foreach ($url in $Urls) {
        foreach ($attempt in 1..3) {
            try {
                Invoke-WebRequest -Uri $url -OutFile $Destination -TimeoutSec 120
                if ((Get-Item -LiteralPath $Destination).Length -gt 0) { return }
            }
            catch {
                $lastError = $_
                Remove-Item -LiteralPath $Destination -Force -ErrorAction SilentlyContinue
                Start-Sleep -Seconds (2 * $attempt)
            }
        }
    }
    throw "Unable to download build asset after retries: $lastError"
}

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$distRoot = [IO.Path]::GetFullPath((Join-Path $repoRoot "dist"))
$outputRoot = [IO.Path]::GetFullPath((Join-Path $repoRoot $OutputDirectory))
if (-not $outputRoot.StartsWith($distRoot + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
    throw "OutputDirectory must be a child of $distRoot"
}

$profile = $Configuration.ToLowerInvariant()
$cargoArguments = @("build", "--locked")
if ($Configuration -eq "Release") {
    $cargoArguments += "--release"
}

Write-Host "Building ACP Bridge ($Configuration)..."
& cargo @cargoArguments --manifest-path (Join-Path $repoRoot "bridge\Cargo.toml")
if ($LASTEXITCODE -ne 0) { throw "ACP Bridge build failed" }

Write-Host "Building Iced configuration UI ($Configuration)..."
& cargo @cargoArguments --manifest-path (Join-Path $repoRoot "desktop\Cargo.toml")
if ($LASTEXITCODE -ne 0) { throw "Iced UI build failed" }

if (Test-Path -LiteralPath $outputRoot) {
    Remove-Item -LiteralPath $outputRoot -Recurse -Force
}
New-Item -ItemType Directory -Path $outputRoot | Out-Null
$runtimeRoot = New-Item -ItemType Directory -Path (Join-Path $outputRoot "runtime")
$resourcesRoot = New-Item -ItemType Directory -Path (Join-Path $outputRoot "resources")
$licensesRoot = New-Item -ItemType Directory -Path (Join-Path $resourcesRoot "licenses")

Copy-Item -LiteralPath (Join-Path $repoRoot "bridge\target\$profile\deerflow-acp.exe") -Destination $outputRoot
Copy-Item -LiteralPath (Join-Path $repoRoot "desktop\target\$profile\deerflow-config.exe") -Destination $outputRoot
Copy-Item -LiteralPath (Join-Path $repoRoot "resources\default-config.yaml") -Destination $resourcesRoot
Copy-Item -LiteralPath (Join-Path $repoRoot "LICENSE") -Destination (Join-Path $licensesRoot "DeerFlow.txt")
Copy-Item -LiteralPath (Join-Path $repoRoot "PORTABLE_README.md") -Destination (Join-Path $outputRoot "README.md")
Copy-Item -LiteralPath (Join-Path $repoRoot "skills") -Destination $resourcesRoot -Recurse

$cacheRoot = Join-Path $repoRoot ".build-cache"
New-Item -ItemType Directory -Path $cacheRoot -Force | Out-Null
$pythonArchive = Join-Path $cacheRoot "python-$PythonVersion-embed-amd64.zip"
$pythonUrl = "https://www.python.org/ftp/python/$PythonVersion/python-$PythonVersion-embed-amd64.zip"
if (-not (Test-Path -LiteralPath $pythonArchive)) {
    Write-Host "Downloading Embedded Python $PythonVersion..."
    Get-CachedRemoteFile -Urls @($pythonUrl) -Destination $pythonArchive
}
Expand-Archive -LiteralPath $pythonArchive -DestinationPath $runtimeRoot -Force

$pythonMinor = ($PythonVersion.Split('.')[0..1] -join '')
$pthFile = Join-Path $runtimeRoot "python$pythonMinor._pth"
if (-not (Test-Path -LiteralPath $pthFile)) {
    throw "Embedded Python path file not found: $pthFile"
}
@(
    "python$pythonMinor.zip"
    "."
    "Lib\site-packages"
    "import site"
) | Set-Content -LiteralPath $pthFile -Encoding ascii

$sitePackages = Join-Path $runtimeRoot "Lib\site-packages"
New-Item -ItemType Directory -Path $sitePackages -Force | Out-Null
Write-Host "Installing DeerFlow and its locked runtime dependencies..."
$requirements = Join-Path $cacheRoot "deerflow-portable-requirements.txt"
& uv export --quiet --frozen --no-dev --no-emit-project --extra rustfs --prune agent-sandbox --output-file $requirements
if ($LASTEXITCODE -ne 0) { throw "Locked dependency export failed" }
& uv pip install --quiet --target $sitePackages --python-version ($PythonVersion.Split('.')[0..1] -join '.') --python-platform x86_64-pc-windows-msvc --link-mode copy --requirements $requirements
if ($LASTEXITCODE -ne 0) { throw "Embedded Python dependency installation failed" }
& uv pip install --quiet --target $sitePackages --python-version ($PythonVersion.Split('.')[0..1] -join '.') --python-platform x86_64-pc-windows-msvc --link-mode copy --no-deps $repoRoot
if ($LASTEXITCODE -ne 0) { throw "DeerFlow package installation failed" }

$forbiddenRuntimePackages = @(
    "agent_sandbox"
    "agent_sandbox-*.dist-info"
    "volcenginesdk*"
    "volcengine_python_sdk-*.dist-info"
    "playwright"
    "playwright-*.dist-info"
    "patchright"
    "patchright-*.dist-info"
)
foreach ($pattern in $forbiddenRuntimePackages) {
    if (Get-ChildItem -LiteralPath $sitePackages -Filter $pattern -Force) {
        throw "Portable Local-only runtime unexpectedly contains: $pattern"
    }
}

$pywin32Chm = Join-Path $sitePackages "PyWin32.chm"
if (Test-Path -LiteralPath $pywin32Chm) {
    Remove-Item -LiteralPath $pywin32Chm -Force
}

Write-Host "Pre-compiling Python bytecode (.pyc)..."
$pythonExe = Join-Path $runtimeRoot "python.exe"
$libRoot = Join-Path $runtimeRoot "Lib"
& $pythonExe -m compileall -q $libRoot
if ($LASTEXITCODE -ne 0) { throw "Python bytecode pre-compilation failed" }

$pythonLicenseCache = Join-Path $cacheRoot "python-$PythonVersion-LICENSE.txt"
Get-CachedRemoteFile -Urls @(
    "https://raw.githubusercontent.com/python/cpython/v$PythonVersion/LICENSE",
    "https://raw.githubusercontent.com/python/cpython/refs/tags/v$PythonVersion/LICENSE"
) -Destination $pythonLicenseCache
Copy-Item -LiteralPath $pythonLicenseCache -Destination (Join-Path $licensesRoot "Python.txt")

foreach ($relative in @("config", "data", "skills", "logs", "backups", "runtime\acp")) {
    New-Item -ItemType Directory -Path (Join-Path $outputRoot "user-data\$relative") -Force | Out-Null
}

Write-Host "Validating embedded Python modules..."
$validationConfig = Join-Path $resourcesRoot "default-config.yaml"
$previousConfigPath = $env:DEER_FLOW_CONFIG_PATH
try {
    # Import validation runs from the repository root, where a developer's
    # config.yaml may be older than the freshly packaged default. Point the
    # child process at the packaged config so validation is deterministic.
    $env:DEER_FLOW_CONFIG_PATH = $validationConfig
    & (Join-Path $runtimeRoot "python.exe") -c "import importlib.util; import logging; import sys; import boto3; import botocore.config; import deerflow.config_tool; import deerflow.acp.daemon; from deerflow.community.scrapling.tools import _scrapling_fetch, web_fetch_tool; assert importlib.util.find_spec('agent_sandbox') is None; assert importlib.util.find_spec('playwright') is None; assert importlib.util.find_spec('patchright') is None; logging.disable(logging.CRITICAL); sys.modules['scrapling.fetchers'] = None; assert _scrapling_fetch('https://invalid.invalid', 1, None).startswith('Error: Scrapling fetch failed: ModuleNotFoundError'); print('embedded Local-only runtime ok')"
    if ($LASTEXITCODE -ne 0) { throw "Embedded Python validation failed" }
}
finally {
    if ($null -eq $previousConfigPath) {
        Remove-Item Env:DEER_FLOW_CONFIG_PATH -ErrorAction SilentlyContinue
    }
    else {
        $env:DEER_FLOW_CONFIG_PATH = $previousConfigPath
    }
}

if (-not $SkipZip) {
    $zipPath = Join-Path (Split-Path $outputRoot -Parent) "DeerFlow-windows-x64.zip"
    if (Test-Path -LiteralPath $zipPath) {
        Remove-Item -LiteralPath $zipPath -Force
    }
    Compress-Archive -LiteralPath $outputRoot -DestinationPath $zipPath -CompressionLevel Optimal
    Write-Host "Portable ZIP: $zipPath"

}
Write-Host "Portable directory: $outputRoot"
