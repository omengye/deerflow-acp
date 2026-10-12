[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
. (Join-Path $PSScriptRoot 'go-test-batches.ps1')
$batchFixtureState = @{ checks = 0; mode = 'pass'; invocations = [Collections.Generic.List[string]]::new() }
function Assert-Batch([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw $Message }
    $batchFixtureState.checks++
}
function Assert-BatchFailure([scriptblock]$Operation, [string]$Expected) {
    $message = ''
    try { & $Operation | Out-Null } catch { $message = $_.Exception.Message }
    Assert-Batch ($message -like "*$Expected*") "Expected '$Expected', got '$message'"
}
foreach ($size in @(0, 1, 32, 33, 266)) {
    $names = @(for ($i = 0; $i -lt $size; $i++) { "TestCase$i" })
    $batches = @(Get-GoTestBatches -Names $names)
    $flattened = @($batches | ForEach-Object { $_.Names })
    Assert-Batch (($flattened -join ',') -ceq ($names -join ',')) "Inventory changed at size $size"
    Assert-Batch ($batches.Count -eq [Math]::Max(1, [Math]::Ceiling($size / 32))) "Wrong batch count at $size"
    foreach ($batch in $batches) {
        Assert-Batch ($batch.Names.Count -le 32) 'Oversized batch'
        $pattern = Get-GoTestPattern -Names $batch.Names
        $results = [ordered]@{}
        foreach ($name in $batch.Names) {
            Assert-Batch ($name -cmatch $pattern -and "${name}Extra" -cnotmatch $pattern) 'Unanchored test pattern'
            $results[$name] = 'pass'
        }
        Assert-GoTestCoverage -Names $batch.Names -Results $results
    }
}
$inventory = @(Get-GoTestInventory @('TestHelperProcess', 'ExampleRun', 'FuzzParse', "ok`tfixture`t0.01s"))
Assert-Batch (($inventory -join ',') -ceq 'TestHelperProcess,ExampleRun,FuzzParse') 'Helper/example/fuzz inventory lost'
Assert-Batch (@(Get-GoTestInventory @('? fixture [no test files]')).Count -eq 0) 'No-test package inventory'
Assert-Batch ((Get-GoTestPattern @()) -ceq '^$') 'Empty inventory selected all tests'
Assert-BatchFailure { Get-GoTestInventory @('TestOne', 'TestOne') } 'Duplicate'
Assert-BatchFailure { Get-GoTestInventory @('unexpected output') } 'Unexpected'
Assert-BatchFailure { Assert-GoTestCoverage @('TestOne') ([ordered]@{}) } 'Missing'
Assert-BatchFailure { Assert-GoTestCoverage @('TestOne') ([ordered]@{ TestOne = 'fail' }) } 'failed'
Assert-BatchFailure { Assert-GoTestCoverage @() ([ordered]@{ TestExtra = 'pass' }) } 'Unexpected'
Assert-GoTestCoverage @('TestSkip') ([ordered]@{ TestSkip = 'skip' })

# Exercise the real runner with a scoped native-command fixture. Coverage
# errors, nonzero exits and duplicate events must block release packaging.
$root = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '..')).Path
$cache = [IO.Path]::GetFullPath((Join-Path $root '.build-cache'))
$fixture = Join-Path $cache ('go-test-runner-' + [Guid]::NewGuid().ToString('N'))
$scripts = Join-Path $fixture 'scripts'
$null = New-Item -ItemType Directory -Path $scripts -Force
$null = New-Item -ItemType Directory -Path (Join-Path $fixture 'go-harness')
function go {
    $global:LASTEXITCODE = 0
    if ($args[0] -eq 'version') { 'go version fixture'; return }
    if ($args[0] -eq 'list') { 'fixture/pkg'; return }
    if ($args -contains '-list=^(Test|Example|Fuzz)') { 'TestOne'; 'TestTwo'; "ok`tfixture/pkg`t0.01s"; return }
    Assert-Batch ((Get-Location).Path -eq (Join-Path $fixture 'go-harness')) 'Runner used wrong module directory'
    Assert-Batch ($args -contains '-p=1' -and $args -contains '-count=1' -and $args -contains '-json') 'Runner lost serial uncached JSON flags'
    $pattern = (($args | Where-Object { $_ -like '-run=*' }) -replace '^-run=', '')
    $batchFixtureState.invocations.Add($pattern)
    foreach ($name in @('TestOne', 'TestTwo')) {
        if ($name -cnotmatch $pattern) { continue }
        if ($batchFixtureState.mode -eq 'missing') { continue }
        $action = if ($name -eq 'TestTwo') { 'skip' } else { 'pass' }
        if ($batchFixtureState.mode -eq 'failed_test') { $action = 'fail' }
        @{ Action = 'pass'; Test = "$name/subtest" } | ConvertTo-Json -Compress
        $event = @{ Action = $action; Test = $name } | ConvertTo-Json -Compress
        $event
        if ($batchFixtureState.mode -eq 'duplicate') { $event }
    }
    if ($batchFixtureState.mode -eq 'exit') { $global:LASTEXITCODE = 42 }
}
$oldProcs = $env:GOMAXPROCS
$oldLocation = (Get-Location).Path
try {
    foreach ($file in @('test-go-harness.ps1', 'go-test-batches.ps1')) {
        Copy-Item -LiteralPath (Join-Path $PSScriptRoot $file) -Destination $scripts
    }
    $entry = Join-Path $scripts 'test-go-harness.ps1'
    $env:GOMAXPROCS = '7'
    & $entry -BatchSize 1
    Assert-Batch (($batchFixtureState.invocations -join ',') -ceq '^(TestOne)$,^(TestTwo)$') 'Runner omitted or repeated a batch'
    $manifestPath = @(Get-ChildItem (Join-Path $fixture '.build-cache/go-tests') -Filter manifest.json -Recurse)[0].FullName
    $manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
    Assert-Batch ($manifest.status -eq 'passed' -and $manifest.packages[0].batches[1].results.TestTwo -eq 'skip') 'Manifest lost skip evidence'
    foreach ($case in @(@('missing', 'Missing'), @('failed_test', 'Go test failed'), @('duplicate', 'Duplicate'), @('exit', 'failed (exit 42)'))) {
        $batchFixtureState.mode = $case[0]
        Assert-BatchFailure { & $entry -BatchSize 1 } $case[1]
        Assert-Batch ($env:GOMAXPROCS -eq '7' -and (Get-Location).Path -eq $oldLocation) 'Failure changed caller environment'
    }
    $failed = @(Get-ChildItem (Join-Path $fixture '.build-cache/go-tests') -Filter manifest.json -Recurse | ForEach-Object {
        Get-Content -LiteralPath $_.FullName -Raw | ConvertFrom-Json
    } | Where-Object status -eq 'failed')
    Assert-Batch ($failed.Count -eq 4) 'Failed runs did not retain manifests'
    Assert-Batch (@($failed | Where-Object { $_.packages[0].batches[0].status -ne 'failed' }).Count -eq 0) 'Failed batch retained running status'
} finally {
    $env:GOMAXPROCS = $oldProcs
    $global:LASTEXITCODE = 0
    $resolved = [IO.Path]::GetFullPath($fixture)
    if (-not $resolved.StartsWith($cache + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) { throw 'Runner fixture cleanup escaped cache' }
    Remove-Item -LiteralPath $resolved -Recurse -Force
}
Write-Host "Go test batching: $($batchFixtureState.checks) inventory, boundary, anchoring and fail-closed checks passed."
