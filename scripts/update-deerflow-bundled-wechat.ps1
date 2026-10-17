#Requires -Version 7.0
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$TargetDirectory,
    [Parameter(Mandatory)][ValidatePattern('^[a-fA-F0-9]{64}$')][string]$ExpectedSha256
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$target = (Resolve-Path -LiteralPath $TargetDirectory).Path.TrimEnd('\')
$plugin = Join-Path $target 'deerflow-channel-wechat.exe'
$control = Join-Path $target 'deerflow-control-go.exe'
$runtime = Join-Path $target 'user-data\runtime\acp-go'
$data = Join-Path $target 'user-data\data\go-harness'
if ((Get-FileHash -LiteralPath $plugin -Algorithm SHA256).Hash -ine $ExpectedSha256) { throw 'Bundled WeChat executable does not match the reviewed build' }

# An explicit invocation approves this bundled executable, not an arbitrary plugin.
$status = $null
for ($i = 0; $i -lt 60; $i++) {
    try {
        $status = '{"operation":"daemon.status"}' | & $control --runtime-dir $runtime 2>$null | ConvertFrom-Json
        if ($LASTEXITCODE -eq 0 -and $status.ok) { break }
    } catch { $status = $null }
    Start-Sleep -Seconds 1
}
if (-not $status -or -not $status.ok) { throw 'Daemon is not ready for plugin registration' }
if ($status.runtime.data_dir -ine $data -or -not $status.runtime.instance_id) { throw 'Unexpected daemon identity or data directory' }
$instance = $status.runtime.instance_id
function Invoke-ChannelManagement([hashtable]$Request) {
    $response = ($Request | ConvertTo-Json -Depth 8 -Compress) | & $control --runtime-dir $runtime --expect-instance-id $instance | ConvertFrom-Json
    if ($LASTEXITCODE -ne 0 -or -not $response.ok) { throw 'Channel management request failed; original credentials and configuration were not replaced' }
    return $response.data
}
$before = Invoke-ChannelManagement @{operation='channel.status'; id='wechat'}
if ($before.id -ne 'wechat' -or $before.descriptor.id -ne 'wechat') { throw 'Expected the reviewed bundled WeChat channel' }
$updated = Invoke-ChannelManagement @{
    operation='channel.update'; id='wechat'; revision=$before.revision
    manifest=@{id='wechat'; command=$plugin; sha256=$ExpectedSha256.ToLowerInvariant()}
}
if (-not $updated.updated) { throw 'Host did not confirm the plugin update' }
$after = Invoke-ChannelManagement @{operation='channel.status'; id='wechat'}
foreach ($field in @('enabled', 'workspace', 'authenticated', 'bound', 'account', 'owner', 'approval', 'full_access_authorized')) {
    if ($before.$field -cne $after.$field) { throw "Channel state changed during registration: $field" }
}
if (($before.options | ConvertTo-Json -Compress) -cne ($after.options | ConvertTo-Json -Compress)) { throw 'Channel options changed during registration' }
[ordered]@{id='wechat'; updated=$true; revision=$after.revision; sha256=$ExpectedSha256.ToLowerInvariant(); configuration_preserved=$true; authentication_preserved=$true} | ConvertTo-Json -Compress
