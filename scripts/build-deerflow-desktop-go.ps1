[CmdletBinding()]
param(
    [ValidateSet("Debug", "Release")]
    [string]$Configuration = "Debug",
    [string]$OutputDirectory = "",
    [string]$DesktopTargetDirectory = "desktop-app\target",
    [switch]$SkipBuild,
    [switch]$CreateZip
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest
$repoRoot = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot "..")).Path
$desktopRoot = Join-Path $repoRoot "desktop-app"
$distRoot = [IO.Path]::GetFullPath((Join-Path $repoRoot "dist\desktop"))
function Resolve-BuildPath([string]$Base, [string]$Value) {
    if ([IO.Path]::IsPathRooted($Value)) { return [IO.Path]::GetFullPath($Value) }
    return [IO.Path]::GetFullPath((Join-Path $Base $Value))
}
$buildId = Get-Date -Format "yyyyMMdd-HHmmss-fff"
if (-not $OutputDirectory) { $OutputDirectory = "dist\desktop\DeerFlow-Desktop-Go-$buildId" }
$outputRoot = Resolve-BuildPath $repoRoot $OutputDirectory
if (-not $outputRoot.StartsWith($distRoot + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
    throw "OutputDirectory must be a child of $distRoot"
}
if (Test-Path -LiteralPath $outputRoot) { throw "Output directory already exists: $outputRoot" }
if ($env:OS -ne "Windows_NT") { throw "This script packages Windows x64 builds." }

$profile = $Configuration.ToLowerInvariant()
$cargoTargetRoot = Resolve-BuildPath $repoRoot $DesktopTargetDirectory
$desktopBinaryRoot = Join-Path $cargoTargetRoot $profile
$bridgeBinary = Join-Path $repoRoot "bridge\target\$profile\deerflow-acp.exe"
$goBinaryRoot = Join-Path $repoRoot ".build-cache\go-acp-desktop\$profile"
$goDaemonBinary = Join-Path $goBinaryRoot "deerflow-acpd.exe"
$goConfigBinary = Join-Path $goBinaryRoot "deerflow-config-go.exe"
$goStdioBinary = Join-Path $goBinaryRoot "deerflow-acp-go.exe"
$cargoArguments = @("build", "--locked")
if ($Configuration -eq "Release") { $cargoArguments += "--release" }
$bridgeCargoArguments = $cargoArguments + @("--config", (Join-Path $PSScriptRoot "cargo-windows-static-crt.toml"))

if (-not $SkipBuild) {
    Push-Location $desktopRoot
    try {
        & cargo @cargoArguments --target-dir $cargoTargetRoot --package waku --bin waku --package waku-daemon --bin waku-daemon
        if ($LASTEXITCODE -ne 0) { throw "Waku Desktop build failed" }
    } finally { Pop-Location }
    Push-Location $repoRoot
    try {
        & cargo @bridgeCargoArguments --manifest-path (Join-Path $repoRoot "bridge\Cargo.toml") --target-dir (Join-Path $repoRoot "bridge\target")
        if ($LASTEXITCODE -ne 0) { throw "ACP Bridge build failed" }
    } finally { Pop-Location }
    New-Item -ItemType Directory -Path $goBinaryRoot -Force | Out-Null
    Push-Location (Join-Path $repoRoot "go-harness")
    try {
        & go build -trimpath -p=2 -o $goDaemonBinary ./cmd/deerflow-acpd-go
        if ($LASTEXITCODE -ne 0) { throw "Go ACP daemon build failed" }
        & go build -trimpath -p=2 -o $goConfigBinary ./cmd/deerflow-config-go
        if ($LASTEXITCODE -ne 0) { throw "Go desktop config build failed" }
        & go build -trimpath -p=2 -o $goStdioBinary ./cmd/deerflow-acp-go
        if ($LASTEXITCODE -ne 0) { throw "Go direct stdio agent build failed" }
    } finally { Pop-Location }
}

$binaries = @{
    "deerflow-desktop.exe" = Join-Path $desktopBinaryRoot "waku.exe"
    "waku-daemon.exe" = Join-Path $desktopBinaryRoot "waku-daemon.exe"
    "deerflow-acp.exe" = $bridgeBinary
    "deerflow-acpd.exe" = $goDaemonBinary
    "deerflow-config-go.exe" = $goConfigBinary
    "deerflow-acp-go.exe" = $goStdioBinary
}
foreach ($name in $binaries.Keys) {
    $source = $binaries[$name]
    if (-not (Test-Path -LiteralPath $source -PathType Leaf) -or (Get-Item -LiteralPath $source).Length -eq 0) {
        throw "Missing or empty build output: $source"
    }
}
$desktopDefault = Join-Path $repoRoot "resources\desktop-default-config.yaml"
if (-not (Test-Path -LiteralPath $desktopDefault -PathType Leaf)) { throw "Missing desktop default config" }

# Create output only after all builds succeed. Never copy data or credentials
# from an older portable installation.
New-Item -ItemType Directory -Path $outputRoot | Out-Null
$packageResources = Join-Path $outputRoot "resources"
New-Item -ItemType Directory -Path $packageResources | Out-Null
foreach ($name in $binaries.Keys) { Copy-Item -LiteralPath $binaries[$name] -Destination (Join-Path $outputRoot $name) }
Copy-Item -LiteralPath $desktopDefault -Destination (Join-Path $packageResources "default-config.yaml")
Copy-Item -LiteralPath (Join-Path $repoRoot "docs\go-native-tools.md") -Destination (Join-Path $packageResources "go-native-tools.md")

function Copy-BundledSkills([string]$Source, [string]$Destination) {
    New-Item -ItemType Directory -Path $Destination -Force | Out-Null
    foreach ($entry in Get-ChildItem -LiteralPath $Source -Force) {
        if (($entry.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { continue }
        if ($entry.PSIsContainer) {
            if ($entry.Name -notin @(".git", ".venv", "node_modules", "__pycache__", ".cache", "user-data", "logs")) {
                Copy-BundledSkills $entry.FullName (Join-Path $Destination $entry.Name)
            }
        } elseif ($entry.Name -notmatch '^\.env($|\.(?!example$))' -and $entry.Extension -notin @(".pyc", ".pyo", ".key", ".pfx", ".p12", ".log", ".db", ".sqlite")) {
            Copy-Item -LiteralPath $entry.FullName -Destination $Destination
        }
    }
}
Copy-BundledSkills (Join-Path $repoRoot "skills") (Join-Path $packageResources "skills")
$licenses = Join-Path $packageResources "licenses"
New-Item -ItemType Directory -Path $licenses | Out-Null
Copy-Item -LiteralPath (Join-Path $desktopRoot "LICENSE") -Destination (Join-Path $licenses "Waku-GPL-3.0.txt")
Copy-Item -LiteralPath (Join-Path $repoRoot "LICENSE") -Destination (Join-Path $licenses "DeerFlow.txt")
Copy-Item -LiteralPath (Join-Path $desktopRoot "FORK.md") -Destination $outputRoot

@'
DeerFlow Desktop (Go-only)

Launch deerflow-desktop.exe. The package uses the Go harness, a local Go ACP
daemon and the Rust ACP Bridge. It does not include or require a Python runtime.

Desktop history and preferences: user-data/desktop
DeerFlow config: user-data/config/config.yaml
Desktop MCP servers: user-data/config/client-mcp-servers.json
Go harness state: user-data/data/go-harness
Go daemon endpoint: user-data/runtime/acp-go

The first launch creates user-data. Set a model and API key in DeerFlow Settings.
Models use provider=openai/claude; sandbox uses provider=local. Native tools are
selected by name without Python class paths. See resources/go-native-tools.md.
Optional host_opencli calls an installed OpenCLI/Node.js; browser commands need
the user's OpenCLI browser bridge. It does not require Python.
The optional stdio MCP executable allowlist is in local_acp of config.yaml.
Some bundled Skills contain Python scripts. Those particular scripts require a
separately installed Python interpreter; the Desktop and Go harness do not.
To update, close Desktop and copy user-data into a new package. Do not replace
or delete it. A previous Python session database cannot be opened by Go.

This Waku-derived desktop is GPL-3.0-only. See FORK.md and resources/licenses.
When distributing the binary ZIP, include the matching source ZIP beside it.
'@ | Set-Content -LiteralPath (Join-Path $outputRoot "README.txt") -Encoding utf8
$commit = (& git -C $repoRoot rev-parse HEAD).Trim()
if ($LASTEXITCODE -eq 0) { Set-Content -LiteralPath (Join-Path $outputRoot "BUILD_COMMIT.txt") -Value $commit -Encoding ascii }

# A Go-only package must not accidentally acquire an older Python runtime.
foreach ($forbidden in @("runtime", "python.exe", "pythonw.exe", ".venv", "site-packages")) {
    if (Test-Path -LiteralPath (Join-Path $outputRoot $forbidden)) { throw "Unexpected Python dependency in package: $forbidden" }
}
$forbiddenEntries = Get-ChildItem -LiteralPath $outputRoot -Recurse -Force | Where-Object {
    $_.Name -match '^(python(?:w)?(?:[0-9.]+)?\.exe|pyvenv\.cfg|site-packages|conda-meta|\.venv)$'
}
if ($forbiddenEntries) { throw "Unexpected Python runtime component in package: $($forbiddenEntries[0].FullName)" }
if ($CreateZip) {
    $zipPath = "$outputRoot-windows-x64.zip"
    if (Test-Path -LiteralPath $zipPath) { throw "ZIP already exists: $zipPath" }
    Compress-Archive -LiteralPath $outputRoot -DestinationPath $zipPath -CompressionLevel Optimal
    $sourceZip = "$outputRoot-source.zip"
    if (Test-Path -LiteralPath $sourceZip) { throw "Source ZIP already exists: $sourceZip" }
    & git -C $repoRoot archive --format=zip --output=$sourceZip HEAD
    if ($LASTEXITCODE -ne 0) { throw "Corresponding source ZIP build failed" }
    Write-Host "Go-only Desktop ZIP: $zipPath"
    Write-Host "Corresponding source ZIP: $sourceZip"
}
Write-Host "Go-only Desktop: $(Join-Path $outputRoot 'deerflow-desktop.exe')"
