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
. (Join-Path $PSScriptRoot "desktop-source-identity.ps1")
. (Join-Path $PSScriptRoot "go-package-inputs.ps1")
Assert-GoRepositorySources -RepositoryRoot $repoRoot
Assert-GoBundledSkills -Source (Join-Path $repoRoot 'skills')
if ($CreateZip -and $SkipBuild) {
    throw '-CreateZip cannot use -SkipBuild: existing binaries have no verified source identity. Rebuild without -SkipBuild, or omit -CreateZip for a development package.'
}
# Fail before Cargo/Go or output creation when a corresponding source ZIP
# would omit local changes. Keep this fixed commit throughout the build.
$sourceIdentity = Get-DesktopSourceIdentity -RepositoryRoot $repoRoot -RequireClean:$CreateZip
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
$goSkillBinary = Join-Path $goBinaryRoot "deerflow-skill-go.exe"
$goWSLHelper = Join-Path $goBinaryRoot "deerflow-wsl-helper-linux-amd64"
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
        & go build -trimpath -p=2 -o $goSkillBinary ./cmd/deerflow-skill-go
        if ($LASTEXITCODE -ne 0) { throw "Go skill authoring tool build failed" }
        & (Join-Path $PSScriptRoot "build-wsl-helper.ps1") -OutputDirectory $goBinaryRoot
    } finally { Pop-Location }
}

$binaries = @{
    "deerflow-desktop.exe" = Join-Path $desktopBinaryRoot "waku.exe"
    "waku-daemon.exe" = Join-Path $desktopBinaryRoot "waku-daemon.exe"
    "deerflow-acp.exe" = $bridgeBinary
    "deerflow-acpd.exe" = $goDaemonBinary
    "deerflow-config-go.exe" = $goConfigBinary
    "deerflow-acp-go.exe" = $goStdioBinary
    "deerflow-skill-go.exe" = $goSkillBinary
    "deerflow-wsl-helper-linux-amd64" = $goWSLHelper
    "deerflow-wsl-helper-linux-amd64.sha256" = "$goWSLHelper.sha256"
    "deerflow-wsl-helper-linux-amd64.json" = "$goWSLHelper.json"
}
foreach ($name in $binaries.Keys) {
    $source = $binaries[$name]
    if (-not (Test-Path -LiteralPath $source -PathType Leaf) -or (Get-Item -LiteralPath $source).Length -eq 0) {
        throw "Missing or empty build output: $source"
    }
}
$helperExpectedDigest = [IO.File]::ReadAllText("$goWSLHelper.sha256").Trim()
if ($helperExpectedDigest -cnotmatch '^[a-f0-9]{64}$' -or $helperExpectedDigest -cne (Get-FileHash -LiteralPath $goWSLHelper -Algorithm SHA256).Hash.ToLowerInvariant()) {
    throw "Go WSL helper checksum mismatch; rebuild the helper before packaging"
}
$helperManifest = Get-Content -LiteralPath "$goWSLHelper.json" -Raw | ConvertFrom-Json
if ($helperManifest.sha256 -cne $helperExpectedDigest -or $helperManifest.platform -cne "linux/amd64" -or $helperManifest.protocol -cne "deerflow-wsl-v1") {
    throw "Go WSL helper manifest mismatch; rebuild the helper before packaging"
}
$desktopDefault = Join-Path $repoRoot "resources\desktop-default-config.yaml"
if (-not (Test-Path -LiteralPath $desktopDefault -PathType Leaf)) { throw "Missing desktop default config" }

# Create output only after all builds succeed. Never copy data or credentials
# from an older portable installation.
if ($CreateZip) { Assert-DesktopSourceIdentity -Expected $sourceIdentity }
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
WSL command execution uses the bundled, SHA-256 verified Go Linux helper.
Select a WSL2 distribution explicitly; no guest interpreter is required.
Bundled skill inputs are checked before building; Python scripts and execution
commands cause an explicit build failure. Native skill authoring is available
through deerflow-skill-go.exe.
To update, close Desktop and copy user-data into a new package. Do not replace
or delete it. A previous Python session database cannot be opened by Go.

This Waku-derived desktop is GPL-3.0-only. See FORK.md and resources/licenses.
When distributing the binary ZIP, include the matching source ZIP beside it.
BUILD_SOURCE.json records whether source identity is exact or a development
snapshot. Source ZIP identity checks do not certify Python source removal or
the separate clean-environment release acceptance.
'@ | Set-Content -LiteralPath (Join-Path $outputRoot "README.txt") -Encoding utf8
$sourceAfterBuild = Get-DesktopSourceIdentity -RepositoryRoot $repoRoot
$sourceRecord = Get-DesktopBuildSourceRecord -Before $sourceIdentity -After $sourceAfterBuild -SkipBuild:$SkipBuild -CreateZip:$CreateZip
Set-Content -LiteralPath (Join-Path $outputRoot "BUILD_COMMIT.txt") -Value $sourceRecord.build_commit_label -Encoding ascii

# A Go-only package must not accidentally acquire an older Python runtime.
foreach ($forbidden in @("runtime", "python.exe", "pythonw.exe", ".venv", "site-packages")) {
    if (Test-Path -LiteralPath (Join-Path $outputRoot $forbidden)) { throw "Unexpected Python dependency in package: $forbidden" }
}
$forbiddenEntries = Get-ChildItem -LiteralPath $outputRoot -Recurse -Force | Where-Object {
    $_.Name -match '^(python(?:w)?(?:[0-9.]+)?\.exe|pyvenv\.cfg|site-packages|conda-meta|\.venv)$' -or $_.Extension -in @('.py', '.pyw', '.pyc', '.pyo')
}
if ($forbiddenEntries) { throw "Unexpected Python runtime component in package: $($forbiddenEntries[0].FullName)" }
$sourceRecord['python_source_dependency_inputs'] = 'checked_absent'
$sourceRecord['python_runtime_in_package'] = 'checked_absent'
$sourceRecord | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath (Join-Path $outputRoot "BUILD_SOURCE.json") -Encoding utf8
if ($CreateZip) {
    $zipPath = "$outputRoot-windows-x64.zip"
    if (Test-Path -LiteralPath $zipPath) { throw "ZIP already exists: $zipPath" }
    $sourceZip = "$outputRoot-source.zip"
    Export-DesktopSourceArchive -Identity $sourceIdentity -Destination $sourceZip
    try {
        Compress-Archive -LiteralPath $outputRoot -DestinationPath $zipPath -CompressionLevel Optimal
        Assert-DesktopSourceIdentity -Expected $sourceIdentity
    } catch {
        if (Test-Path -LiteralPath $zipPath -PathType Leaf) { Remove-Item -LiteralPath $zipPath -Force }
        if (Test-Path -LiteralPath $sourceZip -PathType Leaf) { Remove-Item -LiteralPath $sourceZip -Force }
        throw
    }
    Write-Host "Go-only Desktop ZIP: $zipPath"
    Write-Host "Corresponding source ZIP: $sourceZip"
}
Write-Host "Go-only Desktop: $(Join-Path $outputRoot 'deerflow-desktop.exe')"
