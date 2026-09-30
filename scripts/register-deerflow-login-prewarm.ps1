<#
.SYNOPSIS
Registers, inspects, or removes current-user ACP prewarm at Windows logon.

.EXAMPLE
.\register-deerflow-login-prewarm.ps1 install -BundleRoot 'C:\Apps\DeerFlow'
.\register-deerflow-login-prewarm.ps1 status
.\register-deerflow-login-prewarm.ps1 uninstall
#>

[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [ValidateSet('install', 'status', 'uninstall')]
    [string]$Action = 'status',

    [string]$BundleRoot
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if ($env:OS -ne 'Windows_NT') { throw 'Windows Task Scheduler is required.' }
if (-not $env:LOCALAPPDATA) { throw 'LOCALAPPDATA is not available.' }

$taskName = 'DeerFlow Desktop ACP Prewarm'
$stateDir = Join-Path $env:LOCALAPPDATA 'DeerFlow\prewarm'
$launcher = Join-Path $stateDir 'deerflow-login-prewarm.py'
$lastRun = Join-Path $stateDir 'last-run.json'
$source = Join-Path $PSScriptRoot 'deerflow-login-prewarm.py'
$task = Get-ScheduledTask -TaskName $taskName -TaskPath '\' -ErrorAction SilentlyContinue

switch ($Action) {
    'install' {
        if (-not $BundleRoot) { throw 'Install requires -BundleRoot.' }
        $root = [IO.Path]::GetFullPath($BundleRoot)
        foreach ($required in @(
            (Join-Path $root 'deerflow-desktop.exe'),
            (Join-Path $root 'deerflow-acp.exe'),
            (Join-Path $root 'runtime\python.exe'),
            (Join-Path $root 'runtime\pythonw.exe'),
            (Join-Path $root 'user-data\config\config.yaml')
        )) {
            if (-not (Test-Path -LiteralPath $required -PathType Leaf)) {
                throw "Bundle is incomplete: $required"
            }
        }
        if (-not (Test-Path -LiteralPath $source -PathType Leaf)) {
            throw "Prewarm launcher is missing: $source"
        }

        [IO.Directory]::CreateDirectory($stateDir) | Out-Null
        if (-not [string]::Equals($source, $launcher, [StringComparison]::OrdinalIgnoreCase)) {
            Copy-Item -LiteralPath $source -Destination $launcher -Force
        }

        $pythonw = Join-Path $root 'runtime\pythonw.exe'
        $arguments = '"' + $launcher + '" --bundle-root "' + $root.TrimEnd('\') + '"'
        $taskAction = New-ScheduledTaskAction -Execute $pythonw -Argument $arguments -WorkingDirectory $root
        $user = [Security.Principal.WindowsIdentity]::GetCurrent().Name
        $trigger = New-ScheduledTaskTrigger -AtLogOn -User $user
        $principal = New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Limited
        $settings = New-ScheduledTaskSettingsSet -Hidden -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable -MultipleInstances IgnoreNew -ExecutionTimeLimit (New-TimeSpan -Minutes 7)
        Register-ScheduledTask -TaskName $taskName -TaskPath '\' -Action $taskAction -Trigger $trigger -Principal $principal -Settings $settings -Description 'Prewarm the current DeerFlow Desktop ACP daemon after user logon.' -Force | Out-Null
        if (Test-Path -LiteralPath $lastRun -PathType Leaf) {
            Remove-Item -LiteralPath $lastRun -Force
        }
        Write-Output "Registered $taskName for $user using $root"
    }
    'uninstall' {
        if ($task) {
            Unregister-ScheduledTask -TaskName $taskName -TaskPath '\' -Confirm:$false
        }
        foreach ($path in @($launcher, $lastRun)) {
            if (Test-Path -LiteralPath $path -PathType Leaf) {
                Remove-Item -LiteralPath $path -Force
            }
        }
        if ((Test-Path -LiteralPath $stateDir -PathType Container) -and
            -not (Get-ChildItem -LiteralPath $stateDir -Force | Select-Object -First 1)) {
            Remove-Item -LiteralPath $stateDir
        }
        Write-Output "Removed $taskName"
    }
    'status' {
        if ($task) {
            $info = Get-ScheduledTaskInfo -TaskName $taskName -TaskPath '\'
            Write-Output "Task: $($task.State); last Task Scheduler result: $($info.LastTaskResult)"
            Write-Output "Action: $($task.Actions.Execute) $($task.Actions.Arguments)"
        } else {
            Write-Output 'Task: not registered'
        }
        if (Test-Path -LiteralPath $lastRun -PathType Leaf) {
            Get-Content -LiteralPath $lastRun -Raw
        } else {
            Write-Output 'Last prewarm: no result yet'
        }
    }
}
