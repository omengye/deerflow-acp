[CmdletBinding()]
param(
    [ValidateSet('Desktop', 'ACP', 'All')]
    [string]$Package = 'Desktop',
    [ValidateSet('Debug', 'Release')]
    [string]$Configuration = 'Release',
    [string]$OutputDirectory = '',
    [string]$DesktopTargetDirectory = '.build-cache\desktop-build',
    [ValidateRange(1, 60)]
    [int]$TestTimeoutMinutes = 30,
    [switch]$Development,
    [switch]$SkipTests
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$repoRoot = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '..')).Path
. (Join-Path $PSScriptRoot 'desktop-source-identity.ps1')
if ($env:OS -ne 'Windows_NT' -or [Runtime.InteropServices.RuntimeInformation]::OSArchitecture -ne 'X64') {
    throw 'This entry point builds Windows x64 packages on Windows x64.'
}
if ($SkipTests -and -not $Development) { throw '-SkipTests requires -Development; release archives always run regression tests.' }
if (-not $Development -and $Configuration -ne 'Release') { throw 'Distribution archives require Release. Use -Development for a Debug directory.' }
foreach ($command in @('git', 'go', 'cargo', 'rustc')) {
    $null = Get-Command $command -CommandType Application -ErrorAction Stop
}
$sourceIdentity = Get-DesktopSourceIdentity -RepositoryRoot $repoRoot -RequireClean:(-not $Development)
if (-not $OutputDirectory) { $OutputDirectory = 'dist\windows\' + (Get-Date -Format 'yyyyMMdd-HHmmss-fff') }
$outputRoot = if ([IO.Path]::IsPathRooted($OutputDirectory)) {
    [IO.Path]::GetFullPath($OutputDirectory)
} else { [IO.Path]::GetFullPath((Join-Path $repoRoot $OutputDirectory)) }
$distRoot = [IO.Path]::GetFullPath((Join-Path $repoRoot 'dist'))
if (-not $outputRoot.StartsWith($distRoot + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
    throw "OutputDirectory must be below $distRoot"
}
if (Test-Path -LiteralPath $outputRoot) { throw "Output directory already exists: $outputRoot. Choose a new directory." }
$packages = if ($Package -eq 'All') { @('Desktop', 'ACP') } else { @($Package) }
# The desktop assembler deliberately restricts its output to dist/desktop.
if ($packages -contains 'Desktop' -and -not $outputRoot.StartsWith((Join-Path $distRoot 'desktop') + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
    if ($PSBoundParameters.ContainsKey('OutputDirectory') -and $OutputDirectory) {
        throw 'Desktop/All OutputDirectory must be below dist/desktop (for example dist/desktop/mvp).'
    }
    $outputRoot = Join-Path $distRoot ('desktop\windows-' + (Get-Date -Format 'yyyyMMdd-HHmmss-fff'))
}

function Invoke-CheckedNative {
    param([string]$Command, [string[]]$Arguments)
    & $Command @Arguments
    if ($LASTEXITCODE -ne 0) { throw "$Command $($Arguments -join ' ') failed (exit $LASTEXITCODE)." }
}
function Write-ArchiveChecksum([string]$Archive) {
    $digest = (Get-FileHash -LiteralPath $Archive -Algorithm SHA256).Hash.ToLowerInvariant()
    "$digest  $([IO.Path]::GetFileName($Archive))" | Set-Content -LiteralPath "$Archive.sha256" -Encoding ascii
}

# Set target values only for this invocation; cross-compilation settings from
# the caller must not accidentally produce Linux or ARM files with .exe names.
$previousEnvironment = @{}
foreach ($name in @('GOOS', 'GOARCH', 'CGO_ENABLED', 'DEERFLOW_TEST_BRIDGE', 'DEERFLOW_TEST_GO_DAEMON')) {
    $previousEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}
try {
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    $env:CGO_ENABLED = '0'
    # Ignore caller-selected package fixtures during the initial regression run.
    $env:DEERFLOW_TEST_BRIDGE = $null
    $env:DEERFLOW_TEST_GO_DAEMON = $null
    Push-Location (Join-Path $repoRoot 'go-harness')
    try {
        $goVersion = (& go version) -join ' '
        if ($LASTEXITCODE -ne 0) { throw 'Cannot read Go version' }
        Invoke-CheckedNative go @('mod', 'verify')
        if (-not $SkipTests) {
            & (Join-Path $PSScriptRoot 'test-go-harness.ps1') -TestTimeoutMinutes $TestTimeoutMinutes
        }
    } finally { Pop-Location }
    $cargoVersion = (& cargo --version) -join ' '
    if ($LASTEXITCODE -ne 0) { throw 'Cannot read Cargo version' }
    $rustVersion = (& rustc --version) -join ' '
    if ($LASTEXITCODE -ne 0) { throw 'Cannot read Rust version' }
    if (-not $SkipTests) {
        Invoke-CheckedNative cargo @('test', '--locked', '--manifest-path', (Join-Path $repoRoot 'bridge\Cargo.toml'), '--config', (Join-Path $PSScriptRoot 'cargo-windows-static-crt.toml'))
        & (Join-Path $PSScriptRoot 'test-desktop-source-identity.ps1')
    }
    if (-not $Development) { Assert-DesktopSourceIdentity -Expected $sourceIdentity }
    New-Item -ItemType Directory -Path $outputRoot | Out-Null

    foreach ($kind in $packages) {
        $name = if ($kind -eq 'Desktop') { 'DeerFlow-Desktop-Go' } else { 'DeerFlow-ACP-Go' }
        $packageRoot = Join-Path $outputRoot $name
        if ($kind -eq 'Desktop') {
            & (Join-Path $PSScriptRoot 'build-deerflow-desktop-go.ps1') -Configuration $Configuration -OutputDirectory $packageRoot -DesktopTargetDirectory $DesktopTargetDirectory
            $smokeBinary = Join-Path $repoRoot '.build-cache\deerflow-desktop-smoke.exe'
            Push-Location (Join-Path $repoRoot 'go-harness')
            try { Invoke-CheckedNative go @('build', '-trimpath', '-p=2', '-o', $smokeBinary, './cmd/deerflow-desktop-smoke') } finally { Pop-Location }
            Invoke-CheckedNative $smokeBinary @('--package', $packageRoot)
            Invoke-CheckedNative $smokeBinary @('--package', $packageRoot, '--legacy-empty-mcp-policy')
        } else {
            & (Join-Path $PSScriptRoot 'build-go-acp-portable.ps1') -Configuration $Configuration -OutputDirectory $packageRoot
        }
        # Validate the actual bundled Bridge/daemon, including stdio, startup
        # discovery and draft v2, with isolated local fixtures and no API key.
        $env:DEERFLOW_TEST_BRIDGE = Join-Path $packageRoot 'deerflow-acp.exe'
        $env:DEERFLOW_TEST_GO_DAEMON = Join-Path $packageRoot 'deerflow-acpd.exe'
        Push-Location (Join-Path $repoRoot 'go-harness')
        try {
            Invoke-CheckedNative go @('test', '-count=1', '-run', '^TestExistingRustBridge', '-timeout=5m', './cmd/deerflow-acpd-go')
        } finally { Pop-Location }

        $after = Get-DesktopSourceIdentity -RepositoryRoot $repoRoot
        $sourceRecord = Get-DesktopBuildSourceRecord -Before $sourceIdentity -After $after -CreateZip:(-not $Development)
        Set-Content -LiteralPath (Join-Path $packageRoot 'BUILD_COMMIT.txt') -Value $sourceRecord.build_commit_label -Encoding ascii
        # Preserve the desktop assembler's package input checks.
        $sourceRecordPath = Join-Path $packageRoot 'BUILD_SOURCE.json'
        if (Test-Path -LiteralPath $sourceRecordPath) {
            $assemblerRecord = Get-Content -LiteralPath $sourceRecordPath -Raw | ConvertFrom-Json
            foreach ($field in @('python_source_dependency_inputs', 'python_runtime_in_package')) {
                if ($assemblerRecord.PSObject.Properties.Name -contains $field) { $sourceRecord[$field] = $assemblerRecord.$field }
            }
        }
        $sourceRecord | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $sourceRecordPath -Encoding utf8
        [ordered]@{
            schema_version = 1
            package = $kind.ToLowerInvariant()
            platform = 'windows/amd64'
            configuration = $Configuration
            built_at_utc = [DateTimeOffset]::UtcNow.ToString('o')
            source_state = $sourceRecord.source_state
            commit = $sourceIdentity.Commit
            go = $goVersion
            cargo = $cargoVersion
            rustc = $rustVersion
            regression_tests = $(if ($SkipTests) { 'skipped_development' } else { 'passed' })
            packaged_bridge_tests = 'passed'
            desktop_protocol_smoke = $(if ($kind -eq 'Desktop') { 'passed_both_modes' } else { 'not_applicable' })
        } | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath (Join-Path $packageRoot 'BUILD_PACKAGE.json') -Encoding utf8
        $hashLines = foreach ($file in Get-ChildItem -LiteralPath $packageRoot -Recurse -File -Force | Sort-Object FullName) {
            $relative = $file.FullName.Substring($packageRoot.Length + 1).Replace('\', '/')
            $digest = (Get-FileHash -LiteralPath $file.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
            "$digest  $relative"
        }
        $hashLines | Set-Content -LiteralPath (Join-Path $packageRoot 'PACKAGE_FILES.sha256') -Encoding utf8
        if (-not $Development) {
            Assert-DesktopSourceIdentity -Expected $sourceIdentity
            $sourceZip = "$packageRoot-source.zip"
            $binaryZip = "$packageRoot-windows-x64.zip"
            Export-DesktopSourceArchive -Identity $sourceIdentity -Destination $sourceZip
            try {
                Compress-Archive -LiteralPath $packageRoot -DestinationPath $binaryZip -CompressionLevel Optimal
                Assert-DesktopSourceIdentity -Expected $sourceIdentity
                Write-ArchiveChecksum $binaryZip
                Write-ArchiveChecksum $sourceZip
            } catch {
                # Only newly created files owned by this invocation are removed.
                foreach ($path in @($binaryZip, "$binaryZip.sha256", $sourceZip, "$sourceZip.sha256")) {
                    if (Test-Path -LiteralPath $path -PathType Leaf) { Remove-Item -LiteralPath $path -Force }
                }
                throw
            }
        }
        $label = if ($SkipTests) { 'Development (regression skipped)' } else { 'Verified' }
        Write-Host "$label $kind package: $packageRoot"
    }
} finally {
    foreach ($name in $previousEnvironment.Keys) { [Environment]::SetEnvironmentVariable($name, $previousEnvironment[$name], 'Process') }
}
Write-Host "Windows build output: $outputRoot"
