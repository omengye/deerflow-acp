param(
    [string]$Config = (Join-Path $PSScriptRoot 'adapter.toml'),
    [switch]$Check,
    [switch]$Once
)
$ErrorActionPreference = 'Stop'
$binary = Join-Path $PSScriptRoot 'buzz-deerflow-adapter.exe'
if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) {
    $binary = Join-Path $PSScriptRoot 'bin/buzz-deerflow-adapter.exe'
}
if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) {
    throw 'Native adapter is missing. Run build.ps1 or extract the native Buzz ZIP.'
}
if (-not (Test-Path -LiteralPath $Config -PathType Leaf)) {
    throw 'Copy adapter.example.toml to adapter.toml, set your paths and identity, then create .env from .env.example.'
}
$adapterArgs = @('--config', $Config)
if ($Check) { $adapterArgs += '--check' }
if ($Once) { $adapterArgs += '--once' }
& $binary @adapterArgs
exit $LASTEXITCODE
