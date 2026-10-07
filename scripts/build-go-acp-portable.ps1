[CmdletBinding()]
param(
    [ValidateSet("Debug", "Release")]
    [string]$Configuration = "Release",
    [string]$OutputDirectory = "dist\go-acp\windows-x64",
    [switch]$CreateZip
)

$ErrorActionPreference = "Stop"
$repoRoot = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot "..")).Path
$distRoot = [IO.Path]::GetFullPath((Join-Path $repoRoot "dist"))
$outputRoot = if ([IO.Path]::IsPathRooted($OutputDirectory)) {
    [IO.Path]::GetFullPath($OutputDirectory)
} else { [IO.Path]::GetFullPath((Join-Path $repoRoot $OutputDirectory)) }
if (-not $outputRoot.StartsWith($distRoot + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
    throw "OutputDirectory must be below $distRoot"
}
if (Test-Path -LiteralPath $outputRoot) {
    throw "OutputDirectory already exists: $outputRoot. Choose a new path."
}

$profile = $Configuration.ToLowerInvariant()
$cargoArgs = @("build", "--locked", "--manifest-path", (Join-Path $repoRoot "bridge\Cargo.toml"),
    "--config", (Join-Path $PSScriptRoot "cargo-windows-static-crt.toml"),
    "--target-dir", (Join-Path $repoRoot "bridge\target"))
if ($Configuration -eq "Release") { $cargoArgs += "--release" }
& cargo @cargoArgs
if ($LASTEXITCODE -ne 0) { throw "Rust Bridge build failed" }

New-Item -ItemType Directory -Path $outputRoot -Force | Out-Null
$bridgeBinary = Join-Path $repoRoot "bridge\target\$profile\deerflow-acp.exe"
if (-not (Test-Path -LiteralPath $bridgeBinary -PathType Leaf)) {
    throw "Rust Bridge binary missing: $bridgeBinary"
}
Copy-Item -LiteralPath $bridgeBinary -Destination (Join-Path $outputRoot "deerflow-acp.exe")

Push-Location (Join-Path $repoRoot "go-harness")
try {
    & go build -trimpath -p=2 -o (Join-Path $outputRoot "deerflow-acpd.exe") ./cmd/deerflow-acpd-go
    if ($LASTEXITCODE -ne 0) { throw "Go daemon build failed" }
    & go build -trimpath -p=2 -o (Join-Path $outputRoot "deerflow-acp-go.exe") ./cmd/deerflow-acp-go
    if ($LASTEXITCODE -ne 0) { throw "Go stdio agent build failed" }
    & go build -trimpath -p=2 -o (Join-Path $outputRoot "deerflow-skill-go.exe") ./cmd/deerflow-skill-go
    if ($LASTEXITCODE -ne 0) { throw "Go skill authoring tool build failed" }
    & go build -trimpath -p=2 -o (Join-Path $outputRoot "deerflow-state-go.exe") ./cmd/deerflow-state-go
    if ($LASTEXITCODE -ne 0) { throw "Go state backup tool build failed" }
    & (Join-Path $PSScriptRoot "build-wsl-helper.ps1") -OutputDirectory $outputRoot
}
finally {
    Pop-Location
}

Copy-Item -LiteralPath (Join-Path $repoRoot "LICENSE") -Destination (Join-Path $outputRoot "LICENSE.txt")
Copy-Item -LiteralPath (Join-Path $repoRoot "docs\go-acp-portable.md") -Destination (Join-Path $outputRoot "README.md")
Copy-Item -LiteralPath (Join-Path $repoRoot "docs\go-runtime-backup.md") -Destination (Join-Path $outputRoot "go-runtime-backup.md")
Copy-Item -LiteralPath (Join-Path $repoRoot "resources\skill-behavior") -Destination (Join-Path $outputRoot "skill-behavior") -Recurse
foreach ($binary in @("deerflow-acp.exe", "deerflow-acpd.exe", "deerflow-acp-go.exe", "deerflow-skill-go.exe", "deerflow-state-go.exe", "deerflow-wsl-helper-linux-amd64")) {
    if ((Get-Item -LiteralPath (Join-Path $outputRoot $binary)).Length -eq 0) {
        throw "Package binary is empty: $binary"
    }
}

if ($CreateZip) {
    $zip = "$outputRoot.zip"
    if (Test-Path -LiteralPath $zip) { throw "ZIP already exists: $zip" }
    Compress-Archive -LiteralPath $outputRoot -DestinationPath $zip -CompressionLevel Optimal
    Write-Host "Go ACP ZIP: $zip"
}
Write-Host "Go ACP package: $outputRoot"
