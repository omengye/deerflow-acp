#Requires -Version 7.0
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$PackageDirectory,
    [Parameter(Mandatory)][string]$TargetDirectory,
    [Parameter(Mandatory)][string]$BackupDirectory,
    [string]$SkillCreatorExpectedHash = '',
    [switch]$Restart,
    [switch]$RefreshBundledWeChat
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if ($RefreshBundledWeChat -and -not $Restart) { throw 'Refreshing the bundled channel requires -Restart' }
$channelUpdater = Join-Path $PSScriptRoot 'update-deerflow-bundled-wechat.ps1'
if ($RefreshBundledWeChat -and -not (Test-Path -LiteralPath $channelUpdater -PathType Leaf)) { throw 'Bundled channel updater is missing' }
$source = (Resolve-Path -LiteralPath $PackageDirectory).Path.TrimEnd('\')
$target = (Resolve-Path -LiteralPath $TargetDirectory).Path.TrimEnd('\')
$backup = [IO.Path]::GetFullPath($BackupDirectory).TrimEnd('\')
if ($source -eq $target -or $source.StartsWith($target + '\', [StringComparison]::OrdinalIgnoreCase) -or $target.StartsWith($source + '\', [StringComparison]::OrdinalIgnoreCase)) { throw 'Build and installed package must be separate directories' }
if (Test-Path -LiteralPath $backup) { throw 'Backup directory must not exist' }
foreach ($root in @($source, $target)) {
    if ($backup -eq $root -or $backup.StartsWith($root + '\', [StringComparison]::OrdinalIgnoreCase)) { throw 'Backup must be outside both packages' }
    $entries = @(Get-Item -LiteralPath $root) + @(Get-ChildItem -LiteralPath $root -Force -Recurse)
    if ($entries | Where-Object {($_.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0}) { throw "Refusing a package containing reparse points: $root" }
}
if (Test-Path -LiteralPath (Join-Path $source 'user-data')) { throw 'New build must not contain user-data' }
$names = @('deerflow-desktop.exe', 'waku-daemon.exe', 'deerflow-acp.exe', 'deerflow-acpd.exe', 'deerflow-config-go.exe', 'deerflow-acp-go.exe', 'deerflow-skill-go.exe', 'deerflow-control-go.exe', 'deerflow-channel-wechat.exe')
foreach ($name in $names) {
    $file = Get-Item -LiteralPath (Join-Path $source $name)
    if ($file.Length -eq 0) { throw "Empty executable: $name" }
}
$userData = Join-Path $target 'user-data'
if (-not (Test-Path -LiteralPath $userData -PathType Container)) { throw 'Existing user-data is required' }
$creatorRelative = 'skills\public\skill-creator\SKILL.md'
$creator = Join-Path $userData $creatorRelative
if ($SkillCreatorExpectedHash) {
    if ((Get-FileHash -LiteralPath $creator).Hash -ine $SkillCreatorExpectedHash) { throw 'Skill creator changed since review; refusing to overwrite it' }
    if (-not (Test-Path -LiteralPath (Join-Path $source 'resources\skills\public\skill-creator\SKILL.md'))) { throw 'Build lacks skill creator' }
}

function Get-InstalledProcesses {
    @(Get-CimInstance Win32_Process | Where-Object {
        $_.ExecutablePath -and [IO.Path]::GetDirectoryName($_.ExecutablePath).TrimEnd('\') -ieq $target
    })
}
function Get-FileManifest([string]$Root) {
    $manifest = @{}
    foreach ($file in Get-ChildItem -LiteralPath $Root -File -Force -Recurse) {
        $relative = [IO.Path]::GetRelativePath($Root, $file.FullName)
        $manifest[$relative] = (Get-FileHash -LiteralPath $file.FullName -Algorithm SHA256).Hash
    }
    return $manifest
}
function Assert-Manifest([string]$Root, [hashtable]$Expected) {
    $actual = Get-FileManifest $Root
    if ($actual.Count -ne $Expected.Count) { throw "File count changed: $Root" }
    foreach ($relative in $Expected.Keys) {
        if ($actual[$relative] -cne $Expected[$relative]) { throw "File verification failed: $relative" }
    }
}

$processes = Get-InstalledProcesses
if (($processes | Where-Object {$_.Name -eq 'deerflow-channel-wechat.exe'}) -and -not $RefreshBundledWeChat) {
    throw 'The bundled WeChat plugin is running and pins its executable hash. Review its registration, then use -Restart -RefreshBundledWeChat to update the pin through the host.'
}
$runtime = Join-Path $userData 'runtime\acp-go'
$control = Join-Path $target 'deerflow-control-go.exe'
$drained = $false
try {
    if ($processes | Where-Object {$_.Name -eq 'deerflow-acpd.exe'}) {
        $status = '{"operation":"daemon.status"}' | & $control --runtime-dir $runtime | ConvertFrom-Json
        if ($LASTEXITCODE -ne 0 -or -not $status.ok) { throw 'Cannot confirm daemon activity' }
        if ($status.data.active_runs -ne 0 -or $status.data.queued_runs -ne 0) { throw 'Active or queued work exists; wait for it to finish before updating' }
        $status = '{"operation":"daemon.drain"}' | & $control --runtime-dir $runtime | ConvertFrom-Json
        if ($LASTEXITCODE -ne 0 -or -not $status.ok) { throw 'Cannot drain daemon' }
        $drained = $true
        if ($status.data.active_runs -ne 0 -or $status.data.queued_runs -ne 0) { throw 'Work started before drain; update cancelled' }
    }
    foreach ($process in $processes | Where-Object {$_.Name -eq 'deerflow-desktop.exe'}) {
        $ui = Get-Process -Id $process.ProcessId -ErrorAction SilentlyContinue
        if ($ui) { [void]$ui.CloseMainWindow() }
    }
    Start-Sleep -Seconds 3
    foreach ($process in Get-InstalledProcesses | Where-Object {$_.Name -eq 'deerflow-desktop.exe'}) { Stop-Process -Id $process.ProcessId -ErrorAction Stop }
    if (Get-InstalledProcesses | Where-Object {$_.Name -eq 'deerflow-acpd.exe'}) {
        & (Join-Path $target 'deerflow-acp.exe') --stop-daemon --runtime-dir $runtime --config (Join-Path $userData 'config\config.yaml')
        if ($LASTEXITCODE -ne 0) { throw 'Daemon did not stop cleanly' }
    }
    Start-Sleep -Seconds 2
    foreach ($process in Get-InstalledProcesses) { Stop-Process -Id $process.ProcessId -ErrorAction Stop }
    Start-Sleep -Seconds 1
    if (@(Get-InstalledProcesses).Count -ne 0) { throw 'An installed process restarted; no files replaced' }
} catch {
    if ($drained -and (Get-InstalledProcesses | Where-Object {$_.Name -eq 'deerflow-acpd.exe'})) { '{"operation":"daemon.resume"}' | & $control --runtime-dir $runtime | Out-Null }
    throw
}

New-Item -ItemType Directory -Path $backup | Out-Null
$acl = [Security.AccessControl.DirectorySecurity]::new()
$acl.SetAccessRuleProtection($true, $false)
foreach ($sid in @([Security.Principal.WindowsIdentity]::GetCurrent().User, [Security.Principal.SecurityIdentifier]::new('S-1-5-18'), [Security.Principal.SecurityIdentifier]::new('S-1-5-32-544'))) {
    $acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new($sid, 'FullControl', 'ContainerInherit,ObjectInherit', 'None', 'Allow'))
}
Set-Acl -LiteralPath $backup -AclObject $acl
$before = Get-FileManifest $userData
Copy-Item -LiteralPath $userData -Destination (Join-Path $backup 'user-data') -Recurse
Assert-Manifest (Join-Path $backup 'user-data') $before
$before | ConvertTo-Json -Depth 3 | Set-Content -LiteralPath (Join-Path $backup 'user-data-sha256.json') -Encoding utf8NoBOM
$programBackup = Join-Path $backup 'program'
New-Item -ItemType Directory -Path $programBackup | Out-Null
foreach ($entry in Get-ChildItem -LiteralPath $target -Force | Where-Object {$_.Name -ne 'user-data'}) {
    Copy-Item -LiteralPath $entry.FullName -Destination (Join-Path $programBackup $entry.Name) -Recurse
}
$oldPrograms = Get-FileManifest $programBackup
if (@(Get-InstalledProcesses).Count -ne 0) { throw 'An installed process restarted during backup; no files replaced' }
try {
foreach ($entry in Get-ChildItem -LiteralPath $source -Force) {
    if ($entry.PSIsContainer) {
        $destination = Join-Path $target $entry.Name
        New-Item -ItemType Directory -Path $destination -Force | Out-Null
        foreach ($child in Get-ChildItem -LiteralPath $entry.FullName -Force) { Copy-Item -LiteralPath $child.FullName -Destination $destination -Recurse -Force }
    } else { Copy-Item -LiteralPath $entry.FullName -Destination (Join-Path $target $entry.Name) -Force }
}
foreach ($relative in (Get-FileManifest $source).Keys) {
    if ((Get-FileHash -LiteralPath (Join-Path $source $relative)).Hash -cne (Get-FileHash -LiteralPath (Join-Path $target $relative)).Hash) { throw "Program hash mismatch: $relative" }
}
Assert-Manifest $userData $before
if ($SkillCreatorExpectedHash) {
    Copy-Item -LiteralPath (Join-Path $source 'resources\skills\public\skill-creator\SKILL.md') -Destination $creator -Force
    $before[$creatorRelative] = (Get-FileHash -LiteralPath $creator).Hash
}
Assert-Manifest $userData $before
} catch {
    $failure = $_
    foreach ($entry in Get-ChildItem -LiteralPath $programBackup -Force) {
        if ($entry.PSIsContainer) {
            $destination = Join-Path $target $entry.Name
            New-Item -ItemType Directory -Path $destination -Force | Out-Null
            foreach ($child in Get-ChildItem -LiteralPath $entry.FullName -Force) { Copy-Item -LiteralPath $child.FullName -Destination $destination -Recurse -Force }
        } else { Copy-Item -LiteralPath $entry.FullName -Destination (Join-Path $target $entry.Name) -Force }
    }
    if ($SkillCreatorExpectedHash) { Copy-Item -LiteralPath (Join-Path $backup "user-data\$creatorRelative") -Destination $creator -Force }
    throw "Replacement failed; original programs and skill creator restored. User data was not rolled back. Backup: $backup. Cause: $failure"
}
$result = [ordered]@{source=$source; target=$target; backup=$backup; exe_count=$names.Count; user_data_files=$before.Count; user_data_verified=$true; skill_creator_updated=[bool]$SkillCreatorExpectedHash; programs_before=$oldPrograms}
$result | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath (Join-Path $backup 'deployment.json') -Encoding utf8NoBOM
if ($Restart) {
    $desktop = Start-Process -FilePath (Join-Path $target 'deerflow-desktop.exe') -WorkingDirectory $target -WindowStyle Hidden -PassThru
    Write-Output "Desktop started: PID $($desktop.Id)"
    if ($RefreshBundledWeChat) {
        $pluginHash = (Get-FileHash -LiteralPath (Join-Path $source 'deerflow-channel-wechat.exe') -Algorithm SHA256).Hash
        $channelUpdate = & $channelUpdater -TargetDirectory $target -ExpectedSha256 $pluginHash | ConvertFrom-Json
        $channelUpdate | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath (Join-Path $backup 'channel-update.json') -Encoding utf8NoBOM
    }
}
Write-Output "Replaced and verified $($names.Count) EXEs. User data preserved. Backup: $backup"
