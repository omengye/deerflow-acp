#requires -Version 7.0
[CmdletBinding()]
param(
    [string[]]$Packages = @('./...'),
    [ValidateRange(1, 256)][int]$BatchSize = 32,
    [ValidateRange(1, 60)][int]$TestTimeoutMinutes = 30,
    [switch]$Race
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
# Handle native exit codes explicitly so failed tests retain JSON diagnostics.
$PSNativeCommandUseErrorActionPreference = $false
$repoRoot = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '..')).Path
. (Join-Path $PSScriptRoot 'go-test-batches.ps1')
$mode = if ($Race) { 'race' } else { 'normal' }
$logRoot = Join-Path $repoRoot ('.build-cache/go-tests/' + (Get-Date -Format 'yyyyMMdd-HHmmss-fff') + "-$mode-" + [Guid]::NewGuid().ToString('N').Substring(0, 8))
$null = New-Item -ItemType Directory -Path $logRoot -Force
$manifest = [ordered]@{
    schema_version = 1; mode = $mode; status = 'running'
    started_at = [DateTimeOffset]::UtcNow.ToString('o')
    batch_size = $BatchSize; timeout_minutes = $TestTimeoutMinutes
    packages = [Collections.Generic.List[object]]::new()
}
function Save-TestManifest {
    $manifest | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath (Join-Path $logRoot 'manifest.json') -Encoding utf8
}
$previousProcs = $env:GOMAXPROCS
Push-Location (Join-Path $repoRoot 'go-harness')
try {
    if (-not $env:GOMAXPROCS) { $env:GOMAXPROCS = '2' }
    $manifest['go_version'] = (& go version) -join ' '
    if ($LASTEXITCODE -ne 0) { throw 'Cannot read Go version' }
    $packageNames = @(& go list -mod=readonly @Packages)
    if ($LASTEXITCODE -ne 0 -or $packageNames.Count -eq 0) { throw 'Go package discovery failed or returned no packages' }
    $flags = @('-mod=readonly', '-count=1', '-p=1')
    if ($Race) { $flags += '-race' }
    foreach ($packageName in $packageNames) {
        $index = $manifest.packages.Count
        $inventory = @(& go test @flags '-list=^(Test|Example|Fuzz)' $packageName)
        $inventory | Set-Content -LiteralPath (Join-Path $logRoot "$index-inventory.txt") -Encoding utf8
        if ($LASTEXITCODE -ne 0) { throw "Go test inventory failed: $packageName" }
        $names = @(Get-GoTestInventory -Lines $inventory)
        $packageRecord = [ordered]@{ package = $packageName; tests = $names; batches = [Collections.Generic.List[object]]::new() }
        $manifest.packages.Add($packageRecord)
        foreach ($batch in @(Get-GoTestBatches -Names $names -BatchSize $BatchSize)) {
            $batchIndex = $packageRecord.batches.Count
            $pattern = Get-GoTestPattern -Names $batch.Names
            $logName = "$index-$batchIndex.jsonl"
            $record = [ordered]@{
                tests = $batch.Names; pattern = $pattern; log = $logName; status = 'running'
                results = [Collections.Specialized.OrderedDictionary]::new([StringComparer]::Ordinal)
            }
            $packageRecord.batches.Add($record)
            Save-TestManifest
            Write-Host "Go $mode $packageName batch $($batchIndex + 1) ($($batch.Names.Count) top-level tests)"
            $watch = [Diagnostics.Stopwatch]::StartNew()
            $writer = [IO.StreamWriter]::new((Join-Path $logRoot $logName), $false, [Text.UTF8Encoding]::new($false))
            $writer.AutoFlush = $true
            try {
                # Use go test itself (not a test binary) to preserve package cwd,
                # vet, helper subprocesses and normal fuzz seed/example semantics.
                & go test @flags -json '-parallel=2' "-timeout=$($TestTimeoutMinutes)m" "-run=$pattern" $packageName | ForEach-Object {
                    $writer.WriteLine($_)
                    $event = $_ | ConvertFrom-Json
                    if ($event.PSObject.Properties.Name -contains 'Output') { Write-Host -NoNewline $event.Output }
                    if ($event.PSObject.Properties.Name -contains 'Test' -and $event.Test -notlike '*/*' -and $event.Action -in @('pass', 'skip', 'fail')) {
                        if ($record.results.Contains($event.Test)) { throw "Duplicate Go test result: $($event.Test)" }
                        $record.results[$event.Test] = $event.Action
                    }
                }
                $exitCode = $LASTEXITCODE
            } finally {
                $writer.Dispose()
                $record['elapsed_seconds'] = $watch.Elapsed.TotalSeconds
                Save-TestManifest
            }
            $record['exit_code'] = $exitCode
            if ($exitCode -ne 0) { $record.status = 'failed'; throw "Go regression failed (exit $exitCode): $packageName; log: $logName" }
            Assert-GoTestCoverage -Names $batch.Names -Results $record.results
            $record.status = 'passed'
            Save-TestManifest
        }
    }
    $manifest.status = 'passed'
} catch {
    $manifest.status = 'failed'
    $manifest['error'] = $_.Exception.Message
    foreach ($packageRecord in $manifest.packages) {
        foreach ($batchRecord in $packageRecord.batches) {
            if ($batchRecord.status -eq 'running') { $batchRecord.status = 'failed' }
        }
    }
    throw
} finally {
    $manifest['finished_at'] = [DateTimeOffset]::UtcNow.ToString('o')
    Save-TestManifest
    $env:GOMAXPROCS = $previousProcs
    Pop-Location
    Write-Host "Go test manifest: $logRoot/manifest.json"
}
