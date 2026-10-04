[CmdletBinding()]
param(
    [string]$ConfigPath = (Join-Path $PSScriptRoot "config.yaml"),
    [string]$BridgePath = (Join-Path $PSScriptRoot "bridge\target\release\deerflow-acp.exe"),
    [string]$DaemonPath,
    [string]$RuntimeDir = (Join-Path $PSScriptRoot "user-data\runtime\acp-go"),
    [int]$Port
)
$ErrorActionPreference = "Stop"
if ($PSBoundParameters.ContainsKey('Port')) {
    throw "Port-based shutdown has been removed. Use -RuntimeDir to select the Eino Go daemon."
}
$parameters = @{
    Action = "stop"
    ConfigPath = $ConfigPath
    BridgePath = $BridgePath
    RuntimeDir = $RuntimeDir
}
if ($DaemonPath) { $parameters.DaemonPath = $DaemonPath }
& (Join-Path $PSScriptRoot "acp-service.ps1") @parameters
