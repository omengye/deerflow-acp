[CmdletBinding()]
param(
    [ValidateSet("Debug", "Release")]
    [string]$Configuration = "Release",
    [string]$OutputDirectory = "",
    [string]$DesktopTargetDirectory = "",
    [string]$PortableRuntimeDirectory = "dist\portable\DeerFlow",
    [string]$PythonVersion = "3.12.10",
    [switch]$RebuildRuntime,
    [switch]$SkipBuild,
    [switch]$SkipZip
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$desktopRoot = Join-Path $repoRoot "desktop-app"
function Resolve-BuildPath([string]$Base, [string]$Value) {
    if ([IO.Path]::IsPathRooted($Value)) { return [IO.Path]::GetFullPath($Value) }
    return [IO.Path]::GetFullPath((Join-Path $Base $Value))
}
$distRoot = [IO.Path]::GetFullPath((Join-Path $repoRoot "dist"))
$buildId = Get-Date -Format "yyyyMMdd-HHmmss-fff"
if (-not $OutputDirectory) { $OutputDirectory = "dist\desktop\DeerFlow-Desktop-$buildId" }
$outputRoot = (Resolve-BuildPath $repoRoot $OutputDirectory)
if (-not $outputRoot.StartsWith($distRoot + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
    throw "OutputDirectory must be a child of $distRoot"
}
# Never clean or overlay a previously launched package: it can contain user data.
if (Test-Path -LiteralPath $outputRoot) {
    throw "Output directory already exists. Choose a new OutputDirectory; existing packages and user-data are preserved: $outputRoot"
}
if ($env:OS -ne "Windows_NT") { throw "This script packages Windows x64 builds." }
$profile = $Configuration.ToLowerInvariant()
$cargoTargetRoot = if ($DesktopTargetDirectory) {
    Resolve-BuildPath $repoRoot $DesktopTargetDirectory
} elseif ($env:CARGO_TARGET_DIR) {
    Resolve-BuildPath $desktopRoot $env:CARGO_TARGET_DIR
} else { Join-Path $repoRoot ".build-cache\desktop-build" }
$binaryRoot = Join-Path $cargoTargetRoot $profile
$sourceVersionPath = Join-Path $repoRoot "BUILD_VERSION.txt"
$sourceVersion = if (Test-Path -LiteralPath $sourceVersionPath -PathType Leaf) {
    [IO.File]::ReadAllText($sourceVersionPath).Trim()
} else { $null }
$cargoArguments = @("build", "--locked")
if ($Configuration -eq "Release") { $cargoArguments += "--release" }
# Windows PowerShell 5.1 strips embedded quotes from native arguments.
# Pass a TOML file so Cargo receives the static CRT flags intact.
$bridgeCargoArguments = $cargoArguments + @(
    "--config", (Join-Path $PSScriptRoot "cargo-windows-static-crt.toml")
)

if (-not $SkipBuild) {
    Push-Location $desktopRoot
    try {
        # Run inside desktop-app so its static CRT / GPUI cargo configuration applies.
        & cargo @cargoArguments --target-dir $cargoTargetRoot --package waku --bin waku --package waku-daemon --bin waku-daemon
        if ($LASTEXITCODE -ne 0) { throw "DeerFlow Desktop build failed" }
    } finally { Pop-Location }
    Push-Location $repoRoot
    try {
        # The bridge runs before Python, so it cannot rely on runtime/ being
        # on PATH to supply VCRUNTIME140.dll on a fresh Windows installation.
        & cargo @bridgeCargoArguments --manifest-path (Join-Path $repoRoot "bridge\Cargo.toml") --target-dir (Join-Path $repoRoot "bridge\target")
        if ($LASTEXITCODE -ne 0) { throw "DeerFlow ACP bridge build failed" }
    } finally { Pop-Location }
}
foreach ($binary in @("waku.exe", "waku-daemon.exe")) {
    if (-not (Test-Path -LiteralPath (Join-Path $binaryRoot $binary) -PathType Leaf)) {
        throw "Missing build output: $(Join-Path $binaryRoot $binary)"
    }
}
$bridgeBinary = Join-Path $repoRoot "bridge\target\$profile\deerflow-acp.exe"
if (-not (Test-Path -LiteralPath $bridgeBinary -PathType Leaf)) { throw "Missing bridge: $bridgeBinary" }

$runtimeSource = (Resolve-BuildPath $repoRoot $PortableRuntimeDirectory)
if ($RebuildRuntime -or -not (Test-Path -LiteralPath (Join-Path $runtimeSource "runtime\python.exe"))) {
    # The legacy packager cleans its output. Give it a brand new, checked child
    # path rather than any existing distribution or portable user directory.
    $stagingRelative = "dist\desktop-runtime-staging\$([Guid]::NewGuid().ToString('N'))\DeerFlow"
    $runtimeSource = [IO.Path]::GetFullPath((Join-Path $repoRoot $stagingRelative))
    if (-not $runtimeSource.StartsWith($distRoot + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase) -or (Test-Path -LiteralPath $runtimeSource)) {
        throw "Runtime staging must be a new child of dist"
    }
    $previousScmVersion = $env:SETUPTOOLS_SCM_PRETEND_VERSION
    Push-Location $repoRoot
    try {
        if ($sourceVersion) { $env:SETUPTOOLS_SCM_PRETEND_VERSION = $sourceVersion }
        & (Join-Path $PSScriptRoot "build-deerflow-portable.ps1") -Configuration $Configuration -PythonVersion $PythonVersion -OutputDirectory $stagingRelative -SkipZip
    } finally {
        $env:SETUPTOOLS_SCM_PRETEND_VERSION = $previousScmVersion
        Pop-Location
    }
}
foreach ($required in @("runtime\python.exe", "resources\licenses\Python.txt")) {
    if (-not (Test-Path -LiteralPath (Join-Path $runtimeSource $required) -PathType Leaf)) {
        throw "Incomplete portable runtime source: $required"
    }
}
$desktopDefault = Join-Path $repoRoot "resources\desktop-default-config.yaml"
if (-not (Test-Path -LiteralPath $desktopDefault -PathType Leaf)) { throw "Missing desktop default configuration: $desktopDefault" }

New-Item -ItemType Directory -Path $outputRoot | Out-Null
# Copy an allowlist only; the source package may contain real credentials/history.
Copy-Item -LiteralPath (Join-Path $runtimeSource "runtime") -Destination $outputRoot -Recurse
$packageResources = Join-Path $outputRoot "resources"
New-Item -ItemType Directory -Path $packageResources | Out-Null
# Templates and built-in skills must match current source, not an old runtime.
# Never include local skill credentials or generated caches in a release package.
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
Copy-Item -LiteralPath $bridgeBinary -Destination (Join-Path $outputRoot "deerflow-acp.exe")
Copy-Item -LiteralPath (Join-Path $binaryRoot "waku.exe") -Destination (Join-Path $outputRoot "deerflow-desktop.exe")
Copy-Item -LiteralPath (Join-Path $binaryRoot "waku-daemon.exe") -Destination $outputRoot
Copy-Item -LiteralPath $desktopDefault -Destination (Join-Path $outputRoot "resources\default-config.yaml") -Force
$licenses = Join-Path $outputRoot "resources\licenses"
New-Item -ItemType Directory -Path $licenses -Force | Out-Null
Copy-Item -LiteralPath (Join-Path $runtimeSource "resources\licenses\Python.txt") -Destination $licenses
Copy-Item -LiteralPath (Join-Path $desktopRoot "LICENSE") -Destination (Join-Path $licenses "Waku-GPL-3.0.txt")
Copy-Item -LiteralPath (Join-Path $repoRoot "LICENSE") -Destination (Join-Path $licenses "DeerFlow.txt")
Copy-Item -LiteralPath (Join-Path $desktopRoot "FORK.md") -Destination $outputRoot

# Refresh the application package even when reusing an older dependency runtime.
# Only the new package is changed. The source portable directory stays untouched.
$sitePackages = Join-Path $outputRoot "runtime\Lib\site-packages"
$previousScmVersion = $env:SETUPTOOLS_SCM_PRETEND_VERSION
Push-Location $repoRoot
try {
    if ($sourceVersion) { $env:SETUPTOOLS_SCM_PRETEND_VERSION = $sourceVersion }
    & uv pip install --quiet --target $sitePackages --python-version ($PythonVersion.Split('.')[0..1] -join '.') --python-platform x86_64-pc-windows-msvc --link-mode copy --no-deps --reinstall $repoRoot
    if ($LASTEXITCODE -ne 0) { throw "Refreshing packaged DeerFlow code failed" }
} finally {
    $env:SETUPTOOLS_SCM_PRETEND_VERSION = $previousScmVersion
    Pop-Location
}
$metadataDirectories = @(Get-ChildItem -LiteralPath $sitePackages -Directory -Filter "deerflow_api-*.dist-info")
if ($metadataDirectories.Count -ne 1) { throw "Expected one installed DeerFlow distribution" }
$metadata = Join-Path $metadataDirectories[0].FullName "METADATA"
$versionLine = Select-String -LiteralPath $metadata -Pattern '^Version: ' | Select-Object -First 1
if (-not $versionLine) { throw "Packaged DeerFlow version is missing" }
$packageVersion = $versionLine.Line.Substring(9).Trim()
@'
DeerFlow Desktop

Launch deerflow-desktop.exe. The Settings page configures the bundled DeerFlow
ACP runtime using the same native theme as the conversation workbench.

Desktop history and preferences: user-data/desktop
DeerFlow config: user-data/config/config.yaml
DeerFlow execution/checkpoints: user-data/data
DeerFlow daemon state: user-data/runtime/acp

The first launch creates user-data. To update, close Desktop and copy your
user-data directory into a newly extracted package. Do not replace or delete it.
DEER_FLOW_PORTABLE_ROOT can select another complete portable directory (including
runtime, resources and executables) before launch; it is not a data-only profile.
Debug packages default to the build checkout's desktop-app/temp unless this
variable is set to the extracted package directory.

This Waku-derived desktop is GPL-3.0-only. See FORK.md and resources/licenses.
The matching source ZIP is supplied beside this archive. Upstream Waku updates
and analytics are disabled in this fork.
'@ | Set-Content -LiteralPath (Join-Path $outputRoot "README.txt") -Encoding utf8

if (-not $SkipZip) {
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $zipPath = "$outputRoot-windows-x64.zip"
    if (Test-Path -LiteralPath $zipPath) { throw "Archive already exists: $zipPath" }
    [IO.Compression.ZipFile]::CreateFromDirectory($outputRoot, $zipPath, [IO.Compression.CompressionLevel]::Optimal, $true)

    # Use git's tracked/visible source manifest where available. Exported source
    # trees have no .git, so they fall back to the same explicit source roots.
    $sourceZip = "$outputRoot-source.zip"
    if (Test-Path -LiteralPath $sourceZip) { throw "Source archive already exists: $sourceZip" }
    $sourceRoots = @("desktop-app", "bridge", "desktop", "deerflow", "app", "packages", "resources", "skills", "scripts", "tests", "docs")
    $sourceFiles = @("pyproject.toml", "uv.lock", "LICENSE", "PORTABLE_README.md", "README.md")
    $excluded = @(".git", ".claude", ".codex", "target", "temp", "node_modules", "__pycache__", ".venv", "venv", "user-data", ".build-cache", ".waku-cache", ".pytest_cache", ".ruff_cache", "dist", "build", "logs", "backups", "outputs")
    $sourcePaths = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    function Add-SourcePath([string]$File) {
        $fullPath = [IO.Path]::GetFullPath($File)
        if (-not $fullPath.StartsWith($repoRoot + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) { throw "Source path escaped repository: $fullPath" }
        if (-not (Test-Path -LiteralPath $fullPath -PathType Leaf)) { return }
        $relative = $fullPath.Substring($repoRoot.Length + 1).Replace('\', '/')
        $parts = $relative.Split('/')
        if (@($parts | Where-Object { $_ -in $excluded }).Count -gt 0) { return }
        if ($parts[-1] -match '^\.env($|\.(?!example$))' -or $parts[-1] -match '\.(pyc|pyo|pfx|p12|key|log|sqlite|db)(-wal|-shm)?$') { return }
        if ((Get-Item -LiteralPath $fullPath).Attributes -band [IO.FileAttributes]::ReparsePoint) { return }
        $sourcePaths.Add($relative) | Out-Null
    }
    function Add-SourceDirectory([string]$Directory) {
        foreach ($entry in Get-ChildItem -LiteralPath $Directory -Force) {
            if (($entry.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { continue }
            if ($entry.PSIsContainer) {
                if ($entry.Name -notin $excluded) { Add-SourceDirectory $entry.FullName }
            } else { Add-SourcePath $entry.FullName }
        }
    }
    if ((Test-Path -LiteralPath (Join-Path $repoRoot ".git")) -and (Get-Command git -ErrorAction SilentlyContinue)) {
        $manifest = & git -c core.quotepath=false -C $repoRoot ls-files --cached --others --exclude-standard -- @sourceRoots @sourceFiles
        if ($LASTEXITCODE -ne 0) { throw "Source manifest generation failed" }
        foreach ($relative in $manifest) { Add-SourcePath (Join-Path $repoRoot $relative) }
    } else {
        foreach ($directory in $sourceRoots) { Add-SourceDirectory (Join-Path $repoRoot $directory) }
        foreach ($name in $sourceFiles) { Add-SourcePath (Join-Path $repoRoot $name) }
    }
    # These new fork files may be ignored by a developer's global git rules.
    foreach ($required in @("scripts/build-deerflow-desktop.ps1", "scripts/cargo-windows-static-crt.toml", "scripts/test-deerflow-desktop-smoke.py", "docs/desktop-implementation-20260917.md", "resources/desktop-default-config.yaml", "desktop-app/FORK.md", "desktop-app/LICENSE", "desktop-app/Cargo.lock", "desktop-app/Cargo.toml")) {
        Add-SourcePath (Join-Path $repoRoot $required)
        if (-not $sourcePaths.Contains($required)) { throw "Required source file is missing: $required" }
    }
    $archive = [IO.Compression.ZipFile]::Open($sourceZip, [IO.Compression.ZipArchiveMode]::Create)
    try {
        foreach ($relative in ($sourcePaths | Sort-Object)) {
            [IO.Compression.ZipFileExtensions]::CreateEntryFromFile($archive, (Join-Path $repoRoot $relative), $relative, [IO.Compression.CompressionLevel]::Optimal) | Out-Null
        }
        # hatch-vcs cannot discover a version after .git is intentionally omitted.
        # The exported build script uses this exact wheel version as its fallback.
        $versionEntry = $archive.CreateEntry("BUILD_VERSION.txt")
        $writer = [IO.StreamWriter]::new($versionEntry.Open(), [Text.UTF8Encoding]::new($false))
        try { $writer.WriteLine($packageVersion) } finally { $writer.Dispose() }
    } finally { $archive.Dispose() }
    Write-Host "Portable ZIP: $zipPath"
    Write-Host "Corresponding source ZIP: $sourceZip"
}
Write-Host "DeerFlow Desktop: $(Join-Path $outputRoot 'deerflow-desktop.exe')"
