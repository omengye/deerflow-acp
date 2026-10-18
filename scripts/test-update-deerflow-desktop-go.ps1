#Requires -Version 7.0
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$repository = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '..')).Path
$cache = Join-Path $repository '.build-cache'
$fixture = Join-Path $cache ('update-skill-' + [Guid]::NewGuid().ToString('N'))
$source = Join-Path $fixture 'source'
$target = Join-Path $fixture 'target'
$backup = Join-Path $fixture 'backup'
$creator = Join-Path $target 'user-data\skills\public\skill-creator'
$newCreator = Join-Path $source 'resources\skills\public\skill-creator'
function Assert-Test([bool]$Condition, [string]$Message) { if (-not $Condition) { throw $Message } }
try {
    foreach ($path in @($creator, (Join-Path $newCreator 'references'))) { New-Item -ItemType Directory -Path $path -Force | Out-Null }
    $names = @('deerflow-desktop.exe','waku-daemon.exe','deerflow-acp.exe','deerflow-acpd.exe','deerflow-config-go.exe','deerflow-acp-go.exe','deerflow-skill-go.exe','deerflow-control-go.exe','deerflow-channel-wechat.exe')
    foreach ($name in $names) {
        [IO.File]::WriteAllText((Join-Path $source $name), 'new-fixture-not-executable')
        [IO.File]::WriteAllText((Join-Path $target $name), 'old-fixture-not-executable')
    }
    [IO.File]::WriteAllText((Join-Path $creator 'SKILL.md'), 'reviewed old source')
    [IO.File]::WriteAllText((Join-Path $creator 'old.txt'), 'old support')
    [IO.File]::WriteAllText((Join-Path $newCreator 'SKILL.md'), 'new source with references')
    [IO.File]::WriteAllText((Join-Path $newCreator 'references\python-runtime.md'), 'new reference')
    [IO.File]::WriteAllText((Join-Path $target 'user-data\preserve.txt'), 'private fixture data')
    $manifest = @{}
    foreach ($file in Get-ChildItem -LiteralPath $creator -File -Recurse) { $manifest[[IO.Path]::GetRelativePath($creator, $file.FullName)] = (Get-FileHash -LiteralPath $file.FullName).Hash }
    $manifestPath = Join-Path $fixture 'reviewed.json'
    $manifest | ConvertTo-Json | Set-Content -LiteralPath $manifestPath -Encoding utf8NoBOM
    $updater = Join-Path $PSScriptRoot 'update-deerflow-desktop-go.ps1'
    [IO.File]::WriteAllText((Join-Path $creator 'SKILL.md'), 'unreviewed change')
    $rejected = $false
    try { & $updater -PackageDirectory $source -TargetDirectory $target -BackupDirectory $backup -SkillCreatorExpectedManifest $manifestPath } catch { $rejected = $true }
    Assert-Test $rejected 'Unreviewed source was overwritten'
    Assert-Test (-not (Test-Path -LiteralPath $backup)) 'Preflight failure created backup/deployment'
    [IO.File]::WriteAllText((Join-Path $creator 'SKILL.md'), 'reviewed old source')
    & $updater -PackageDirectory $source -TargetDirectory $target -BackupDirectory $backup -SkillCreatorExpectedManifest $manifestPath
    Assert-Test (Test-Path -LiteralPath (Join-Path $creator 'references\python-runtime.md')) 'New reference missing'
    Assert-Test (-not (Test-Path -LiteralPath (Join-Path $creator 'old.txt'))) 'Obsolete reference retained'
    Assert-Test ([IO.File]::ReadAllText((Join-Path $backup 'user-data\skills\public\skill-creator\old.txt')) -eq 'old support') 'Full skill backup missing'
    Assert-Test ([IO.File]::ReadAllText((Join-Path $target 'user-data\preserve.txt')) -eq 'private fixture data') 'Unrelated user data changed'
    foreach ($name in $names) { Assert-Test ((Get-FileHash -LiteralPath (Join-Path $source $name)).Hash -eq (Get-FileHash -LiteralPath (Join-Path $target $name)).Hash) "Executable mismatch: $name" }
    Write-Host 'Updater: complete creator migration, manifest conflict, program hashes and preserved user data passed.'
} finally {
    $resolved = [IO.Path]::GetFullPath($fixture)
    if (-not $resolved.StartsWith([IO.Path]::GetFullPath($cache) + '\', [StringComparison]::OrdinalIgnoreCase)) { throw 'Fixture escaped cache' }
    if (Test-Path -LiteralPath $resolved) { Remove-Item -LiteralPath $resolved -Recurse -Force }
}
