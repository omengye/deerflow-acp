param(
    [string]$Version = '0.3.0',
    [string]$OutputDirectory,
    [string]$DesktopRoot,
    [switch]$Development
)
$ErrorActionPreference = 'Stop'
if ($env:OS -ne 'Windows_NT') { throw 'Use this packaging script on Windows.' }
if ($Version -notmatch '^[0-9A-Za-z][0-9A-Za-z._-]*$') { throw 'Invalid version.' }
$repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
$sourceCommit = (& git -C $repoRoot rev-parse HEAD).Trim()
if ($LASTEXITCODE -ne 0) { throw 'Cannot identify source commit.' }
$dirty = @(& git -C $repoRoot status --porcelain --untracked-files=all).Count -gt 0
if ($dirty -and -not $Development) { throw 'Release ZIP requires committed sources. Commit changes first; use -Development only for local development packages.' }
if (-not $OutputDirectory) {
    $OutputDirectory = Join-Path $repoRoot ("dist/buzz/Buzz-DeerFlow-Go-$Version-windows-x64-" + (Get-Date -Format 'yyyyMMdd-HHmmss'))
}
$OutputDirectory = [IO.Path]::GetFullPath($OutputDirectory)
if (Test-Path -LiteralPath $OutputDirectory) { throw 'Output directory already exists. Select a new directory.' }
$previousBridge = $env:DEERFLOW_TEST_BRIDGE
$previousDaemon = $env:DEERFLOW_TEST_GO_DAEMON
$previousCGO = $env:CGO_ENABLED
$previousGOOS = $env:GOOS
$previousGOARCH = $env:GOARCH
Push-Location $PSScriptRoot
try {
    if ($DesktopRoot) {
        $env:DEERFLOW_TEST_BRIDGE = Join-Path $DesktopRoot 'deerflow-acp.exe'
        $env:DEERFLOW_TEST_GO_DAEMON = Join-Path $DesktopRoot 'deerflow-acpd.exe'
        foreach ($binary in @($env:DEERFLOW_TEST_BRIDGE, $env:DEERFLOW_TEST_GO_DAEMON)) {
            if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) { throw 'DesktopRoot must contain the current native Go release.' }
        }
    }
    & go mod verify
    if ($LASTEXITCODE -ne 0) { throw 'Dependency verification failed.' }
    & go test -count=1 -timeout=3m ./...
    if ($LASTEXITCODE -ne 0) { throw 'Adapter tests failed.' }
    & go vet ./...
    if ($LASTEXITCODE -ne 0) { throw 'Go vet failed.' }
    New-Item -ItemType Directory -Path $OutputDirectory | Out-Null
	$env:CGO_ENABLED = '0'
	$env:GOOS = 'windows'
	$env:GOARCH = 'amd64'
    $exe = Join-Path $OutputDirectory 'buzz-deerflow-adapter.exe'
    & go build -trimpath -ldflags "-s -w -X main.version=$Version" -o $exe ./cmd/buzz-deerflow-adapter
    if ($LASTEXITCODE -ne 0) { throw 'Adapter build failed.' }
    & $exe --version
    if ($LASTEXITCODE -ne 0) { throw 'Native binary smoke check failed.' }
    foreach ($file in @('README.md', 'adapter.example.toml', '.env.example', 'start.bat', 'start.ps1')) {
        Copy-Item -LiteralPath (Join-Path $PSScriptRoot $file) -Destination $OutputDirectory
    }
    Copy-Item -LiteralPath (Join-Path $repoRoot 'LICENSE') -Destination $OutputDirectory
    $buildInfo = @{ version = $Version; source_commit = $sourceCommit; development = [bool]$Development; source_dirty = $dirty; platform = 'windows-amd64'; built_at = [DateTime]::UtcNow.ToString('o'); go_version = (& go version) }
    [IO.File]::WriteAllText((Join-Path $OutputDirectory 'BUILD-INFO.json'), ($buildInfo | ConvertTo-Json), [Text.UTF8Encoding]::new($false))
    $exeHash = (Get-FileHash -LiteralPath $exe -Algorithm SHA256).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText((Join-Path $OutputDirectory 'SHA256SUMS.txt'), "$exeHash  buzz-deerflow-adapter.exe`n", [Text.UTF8Encoding]::new($false))
    $zip = "$OutputDirectory.zip"
    if (Test-Path -LiteralPath $zip) { throw 'ZIP already exists.' }
    Compress-Archive -Path (Join-Path $OutputDirectory '*') -DestinationPath $zip
    $zipHash = (Get-FileHash -LiteralPath $zip -Algorithm SHA256).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText("$zip.sha256", "$zipHash  $([IO.Path]::GetFileName($zip))`n", [Text.UTF8Encoding]::new($false))
    # Keep a source-tree launcher usable without installing a Go runtime later.
    New-Item -ItemType Directory -Force -Path (Join-Path $PSScriptRoot 'bin') | Out-Null
    Copy-Item -LiteralPath $exe -Destination (Join-Path $PSScriptRoot 'bin/buzz-deerflow-adapter.exe') -Force
    Write-Output "ZIP: $zip"
    Write-Output "SHA256: $zipHash"
} finally {
    $env:CGO_ENABLED = $previousCGO
    $env:GOOS = $previousGOOS
    $env:GOARCH = $previousGOARCH
    $env:DEERFLOW_TEST_BRIDGE = $previousBridge
    $env:DEERFLOW_TEST_GO_DAEMON = $previousDaemon
    Pop-Location
}
