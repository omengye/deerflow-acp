[CmdletBinding()]
param(
    [ValidateSet('Debug', 'Release')]
    [string]$Configuration = 'Release',
    [string]$OutputDirectory = '',
    [string]$DesktopTargetDirectory = '.build-cache\desktop-build',
    [switch]$SkipBuild,
    [switch]$SkipZip
)

$ErrorActionPreference = 'Stop'
Write-Warning 'Deprecated entry point: use scripts/build-deerflow-desktop-go.ps1. This wrapper builds only the Eino Go desktop.'
$arguments = @{
    Configuration = $Configuration
    OutputDirectory = $OutputDirectory
    DesktopTargetDirectory = $DesktopTargetDirectory
    SkipBuild = $SkipBuild
    CreateZip = -not $SkipZip
}
& (Join-Path $PSScriptRoot 'build-deerflow-desktop-go.ps1') @arguments
