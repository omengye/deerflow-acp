#Requires -Version 7.0
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
. (Join-Path $PSScriptRoot 'deerflow-data-directory.ps1')
$fixture = Join-Path ([IO.Path]::GetTempPath()) ('deerflow-data-binding-' + [guid]::NewGuid().ToString())
function Assert-Throws([scriptblock]$Action) {
    $threw = $false
    try { & $Action | Out-Null } catch { $threw = $true }
    if (-not $threw) { throw 'Expected directory validation to fail' }
}
try {
    $default = Join-Path $fixture 'config\go-harness-state'
    if ((Get-DeerFlowDataDirectory $fixture) -ne $default) { throw 'Fresh install default changed' }
    $original = Join-Path $fixture 'config\go-harness-state'
    $alternate = Join-Path $fixture 'data\go-harness'
    foreach ($directory in @($original, $alternate)) {
        New-Item -ItemType Directory -Path $directory -Force | Out-Null
        Set-Content -LiteralPath (Join-Path $directory 'harness.db') -Value $directory
    }
    $binding = Join-Path $fixture 'config\runtime-data.json'
    Assert-Throws { Get-DeerFlowDataDirectory $fixture }
    Set-Content -LiteralPath $binding -Value '{"data_dir":"config/go-harness-state"}' -Encoding utf8NoBOM
    Assert-Throws { Get-DeerFlowDataDirectory $fixture }
    Move-Item -LiteralPath (Join-Path $alternate 'harness.db') -Destination (Join-Path $fixture 'archived.db')
    if ((Get-DeerFlowDataDirectory $fixture) -ne $original) { throw 'Original database was not selected' }
    Assert-DeerFlowDataDirectory @{ok=$true;runtime=@{data_dir=$original}} $original
    Assert-Throws { Assert-DeerFlowDataDirectory @{ok=$true;runtime=@{data_dir=$alternate}} $original }
    Assert-Throws { Assert-DeerFlowDataDirectory @{ok=$true;runtime=@{}} $original }
    foreach ($invalid in @('bad json', '{"data_dir":"config/missing"}', '{"data_dir":"../escape"}', '{"data_dir":""}', '{"data_dir":"data/go-harness","extra":true}')) {
        Set-Content -LiteralPath $binding -Value $invalid -Encoding utf8NoBOM
        Assert-Throws { Get-DeerFlowDataDirectory $fixture }
    }
    Write-Output 'PASS: default, explicit selection, runtime mismatch, missing identity, invalid binding and missing database'
} finally {
    $checked = [IO.Path]::GetFullPath($fixture)
    $temporaryRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\') + '\'
    if (-not $checked.StartsWith($temporaryRoot, [StringComparison]::OrdinalIgnoreCase) -or (Split-Path -Leaf $checked) -notlike 'deerflow-data-binding-*') { throw 'Unsafe fixture cleanup path' }
    if (Test-Path -LiteralPath $checked) { Remove-Item -LiteralPath $checked -Recurse -Force }
}
