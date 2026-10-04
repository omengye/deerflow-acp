<#
.SYNOPSIS
Starts and manages the detached DeerFlow ACP daemon.

.DESCRIPTION
This is a convenience entry point over acp-service.ps1. The default action is
`start`. The native Bridge launches the Eino Go daemon without a window, with
null stdio and in a separate process group, so this script returns after the
daemon is ready and the daemon keeps running after PowerShell closes.

No Windows service or scheduled task is created.

.EXAMPLE
.\acp-daemon.ps1
.\acp-daemon.ps1 status
.\acp-daemon.ps1 restart
.\acp-daemon.ps1 stop
#>

[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [ValidateSet("start", "stop", "restart", "status")]
    [string]$Action = "start",

    [string]$ConfigPath = (Join-Path $PSScriptRoot "config.yaml"),
    [string]$BridgePath = (Join-Path $PSScriptRoot "bridge\target\release\deerflow-acp.exe"),
    [string]$DaemonPath,
    [string]$RuntimeDir = (Join-Path $PSScriptRoot "user-data\runtime\acp-go"),
    [string]$PythonPath
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

if ($PSBoundParameters.ContainsKey('PythonPath')) {
    throw "PythonPath has been removed. Use -DaemonPath with the Eino Go deerflow-acpd executable."
}

$parameters = @{
    Action = $Action
    ConfigPath = $ConfigPath
    BridgePath = $BridgePath
}
if ($DaemonPath) {
    $parameters.DaemonPath = $DaemonPath
}
if ($RuntimeDir) {
    $parameters.RuntimeDir = $RuntimeDir
}

& (Join-Path $PSScriptRoot "acp-service.ps1") @parameters
