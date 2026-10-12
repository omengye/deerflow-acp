[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$repoRoot = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '..')).Path
$cacheRoot = [IO.Path]::GetFullPath((Join-Path $repoRoot '.build-cache'))
$fixtureRoot = Join-Path $cacheRoot ('windows-package-' + [Guid]::NewGuid().ToString('N'))
$fixtureRepo = Join-Path $fixtureRoot 'repo with spaces'
$fixtureScripts = Join-Path $fixtureRepo 'scripts'
$null = New-Item -ItemType Directory -Path $fixtureScripts -Force
$null = New-Item -ItemType Directory -Path (Join-Path $fixtureRepo 'go-harness')
$checks = 0
function Assert-PackageFixture([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw "Windows package fixture: $Message" }
    $script:checks++
}
function Assert-PackageFailure([scriptblock]$Operation, [string]$Expected) {
    $message = ''
    try { & $Operation | Out-Null } catch { $message = $_.Exception.Message }
    Assert-PackageFixture ($message -like "*$Expected*") "expected '$Expected'; observed '$message'"
}

# Replace compilers only in this script's scope. Git identity, archiving and
# file hashes use the production code; no model or real build is invoked.
function go {
    if ($args[0] -eq 'version') { $global:LASTEXITCODE = 0; 'go version fixture windows/amd64'; return }
    Add-Content -LiteralPath $env:DEERFLOW_PACKAGE_FIXTURE_TRACE -Value ('go ' + ($args -join ' '))
    $global:LASTEXITCODE = 0
    if ($args[0] -eq 'test') {
        if ($env:DEERFLOW_TEST_BRIDGE) {
            if ($env:DEERFLOW_PACKAGE_FIXTURE_FAILURE -eq 'smoke') { $global:LASTEXITCODE = 43 }
        } elseif ($env:DEERFLOW_PACKAGE_FIXTURE_FAILURE -eq 'regression') { $global:LASTEXITCODE = 42 }
    }
}
function cargo {
    $global:LASTEXITCODE = 0
    if ($args[0] -eq '--version') { 'cargo fixture'; return }
    Add-Content -LiteralPath $env:DEERFLOW_PACKAGE_FIXTURE_TRACE -Value ('cargo ' + ($args -join ' '))
    if ($env:DEERFLOW_PACKAGE_FIXTURE_FAILURE -eq 'bridge') { $global:LASTEXITCODE = 44 }
}
function rustc { $global:LASTEXITCODE = 0; 'rustc fixture' }
$script:tracePath = Join-Path $fixtureRoot 'trace.txt'
$previousGoos = $env:GOOS
$previousBridge = $env:DEERFLOW_TEST_BRIDGE
$previousTrace = $env:DEERFLOW_PACKAGE_FIXTURE_TRACE
$previousFailure = $env:DEERFLOW_PACKAGE_FIXTURE_FAILURE
try {
    $env:DEERFLOW_PACKAGE_FIXTURE_TRACE = $script:tracePath
    $env:DEERFLOW_PACKAGE_FIXTURE_FAILURE = ''
    Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'build-windows.ps1') -Destination $fixtureScripts
    Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'desktop-source-identity.ps1') -Destination $fixtureScripts
    Set-Content -LiteralPath (Join-Path $fixtureScripts 'test-desktop-source-identity.ps1') -Value '# Identity fixture is tested separately.'
    @'
param([int]$TestTimeoutMinutes)
'Go regression runner' | Add-Content -LiteralPath $env:DEERFLOW_PACKAGE_FIXTURE_TRACE
if ($env:DEERFLOW_PACKAGE_FIXTURE_FAILURE -eq 'regression') { throw 'Go regression failed (exit 42)' }
'@ | Set-Content -LiteralPath (Join-Path $fixtureScripts 'test-go-harness.ps1') -Encoding utf8
    @'
param([string]$Configuration,[string]$OutputDirectory)
New-Item -ItemType Directory -Path $OutputDirectory | Out-Null
foreach ($name in @('deerflow-acp.exe','deerflow-acpd.exe','README.md')) {
    Set-Content -LiteralPath (Join-Path $OutputDirectory $name) -Value "fixture $name" -Encoding utf8
}
'assembled ACP' | Add-Content -LiteralPath $env:DEERFLOW_PACKAGE_FIXTURE_TRACE
'@ | Set-Content -LiteralPath (Join-Path $fixtureScripts 'build-go-acp-portable.ps1') -Encoding utf8
    Set-Content -LiteralPath (Join-Path $fixtureRepo '.gitignore') -Value "dist/`ngo-harness/"
    . (Join-Path $fixtureScripts 'desktop-source-identity.ps1')
    $null = Invoke-DesktopSourceGit $fixtureRepo @('init', '--quiet')
    $null = Invoke-DesktopSourceGit $fixtureRepo @('add', '--', '.gitignore', 'scripts')
    $null = Invoke-DesktopSourceGit $fixtureRepo @('-c', 'user.name=Package fixture', '-c', 'user.email=fixture@example.invalid', '-c', 'commit.gpgsign=false', '-c', 'core.hooksPath=nonexistent-fixture-hooks', 'commit', '--quiet', '-m', 'fixture')
    $entry = Join-Path $fixtureScripts 'build-windows.ps1'
    $env:GOOS = 'linux'
    $env:DEERFLOW_TEST_BRIDGE = 'caller-fixture'

    Assert-PackageFailure { & $entry -Package ACP -SkipTests } '-SkipTests requires -Development'
    Assert-PackageFailure { & $entry -Package ACP -OutputDirectory '../escaped' } 'must be below'
    Assert-PackageFailure { & $entry -Package Desktop -OutputDirectory 'dist/wrong' } 'must be below dist/desktop'
    Assert-PackageFailure { & $entry -Package ACP -Configuration Debug } 'Distribution archives require Release'
    $dirtyFile = Join-Path $fixtureRepo 'uncommitted.txt'
    Set-Content -LiteralPath $dirtyFile -Value 'dirty'
    Assert-PackageFailure { & $entry -Package ACP } 'requires clean committed source'
    Assert-PackageFixture (-not (Test-Path -LiteralPath $script:tracePath)) 'preflight failures invoked a compiler'
    Remove-Item -LiteralPath $dirtyFile

    foreach ($mode in @('regression', 'bridge', 'smoke')) {
        $env:DEERFLOW_PACKAGE_FIXTURE_FAILURE = $mode
        $dest = "dist/$mode"
        Assert-PackageFailure { & $entry -Package ACP -OutputDirectory $dest } 'failed (exit'
        $archives = @(Get-ChildItem -LiteralPath (Join-Path $fixtureRepo 'dist') -Filter '*.zip' -Recurse -ErrorAction SilentlyContinue)
        Assert-PackageFixture ($archives.Count -eq 0) "$mode failure generated an archive"
        Assert-PackageFixture ($env:GOOS -eq 'linux' -and $env:DEERFLOW_TEST_BRIDGE -eq 'caller-fixture') "$mode failure changed caller environment"
    }
    $env:DEERFLOW_PACKAGE_FIXTURE_FAILURE = ''
    & $entry -Package ACP -OutputDirectory 'dist/success'
    $output = Join-Path $fixtureRepo 'dist/success'
    $archives = @(Get-ChildItem -LiteralPath $output -Filter '*.zip')
    Assert-PackageFixture ($archives.Count -eq 2) 'expected binary and matching source ZIPs'
    foreach ($archive in $archives) {
        $expected = (Get-FileHash -LiteralPath $archive.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
        $line = (Get-Content -LiteralPath ($archive.FullName + '.sha256') -Raw).Trim()
        Assert-PackageFixture ($line -ceq "$expected  $($archive.Name)") 'archive checksum does not match'
    }
    $packageRoot = Join-Path $output 'DeerFlow-ACP-Go'
    $manifest = Get-Content -LiteralPath (Join-Path $packageRoot 'BUILD_PACKAGE.json') -Raw | ConvertFrom-Json
    Assert-PackageFixture ($manifest.regression_tests -eq 'passed' -and $manifest.packaged_bridge_tests -eq 'passed') 'verified package missing test evidence'
    $identity = Get-Content -LiteralPath (Join-Path $packageRoot 'BUILD_SOURCE.json') -Raw | ConvertFrom-Json
    Assert-PackageFixture ($identity.source_state -eq 'clean-rebuilt' -and $identity.source_archive_commit -eq $manifest.commit) 'source archive has a different commit'
    foreach ($line in Get-Content -LiteralPath (Join-Path $packageRoot 'PACKAGE_FILES.sha256')) {
        $parts = $line -split '  ', 2
        $actual = (Get-FileHash -LiteralPath (Join-Path $packageRoot $parts[1]) -Algorithm SHA256).Hash.ToLowerInvariant()
        Assert-PackageFixture ($parts[0] -ceq $actual) 'package file checksum does not match'
    }
    $expanded = Join-Path $fixtureRoot 'expanded'
    Expand-Archive -LiteralPath (Join-Path $output 'DeerFlow-ACP-Go-windows-x64.zip') -DestinationPath $expanded
    Assert-PackageFixture (Test-Path -LiteralPath (Join-Path $expanded 'DeerFlow-ACP-Go/BUILD_PACKAGE.json')) 'binary ZIP omits post-smoke manifest'
    Assert-PackageFailure { & $entry -Package ACP -OutputDirectory 'dist/success' } 'already exists'
    Set-Content -LiteralPath $dirtyFile -Value 'development'
    & $entry -Package ACP -Development -SkipTests -OutputDirectory 'dist/development'
    $dev = Join-Path $fixtureRepo 'dist/development'
    Assert-PackageFixture (@(Get-ChildItem -LiteralPath $dev -Filter '*.zip').Count -eq 0) 'development build emitted release ZIPs'
    $manifest = Get-Content -LiteralPath (Join-Path $dev 'DeerFlow-ACP-Go/BUILD_PACKAGE.json') -Raw | ConvertFrom-Json
    Assert-PackageFixture ($manifest.regression_tests -eq 'skipped_development' -and $manifest.source_state -eq 'development-dirty') 'development build falsely claims verification'
    Write-Host "Windows packaging: $checks source, failure, archive, checksum and environment checks passed."
} finally {
    $env:GOOS = $previousGoos
    $env:DEERFLOW_TEST_BRIDGE = $previousBridge
    $env:DEERFLOW_PACKAGE_FIXTURE_TRACE = $previousTrace
    $env:DEERFLOW_PACKAGE_FIXTURE_FAILURE = $previousFailure
    $resolvedFixture = [IO.Path]::GetFullPath($fixtureRoot)
    if (-not $resolvedFixture.StartsWith($cacheRoot + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) { throw 'Fixture cleanup escaped build cache' }
    Remove-Item -LiteralPath $resolvedFixture -Recurse -Force
}
