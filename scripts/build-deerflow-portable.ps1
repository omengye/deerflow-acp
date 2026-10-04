[CmdletBinding()]
param(
    [ValidateSet('Debug', 'Release')]
    [string]$Configuration = 'Release',
    [string]$OutputDirectory = 'dist\portable\DeerFlow-Go',
    [switch]$SkipZip
)

$ErrorActionPreference = 'Stop'
Write-Warning 'Deprecated entry point: use scripts/build-go-acp-portable.ps1. This wrapper builds only the Eino Go ACP package.'
& (Join-Path $PSScriptRoot 'build-go-acp-portable.ps1') `
    -Configuration $Configuration -OutputDirectory $OutputDirectory -CreateZip:(-not $SkipZip)
