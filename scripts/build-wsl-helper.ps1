[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$OutputDirectory
)

$ErrorActionPreference = "Stop"
$repoRoot = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot "..")).Path
$helperOutputRoot = if ([IO.Path]::IsPathRooted($OutputDirectory)) {
    [IO.Path]::GetFullPath($OutputDirectory)
} else {
    [IO.Path]::GetFullPath((Join-Path $repoRoot $OutputDirectory))
}
New-Item -ItemType Directory -Path $helperOutputRoot -Force | Out-Null
$helperBinary = Join-Path $helperOutputRoot "deerflow-wsl-helper-linux-amd64"
$helperPreviousGoos = $env:GOOS
$helperPreviousGoarch = $env:GOARCH
$helperPreviousCgo = $env:CGO_ENABLED
Push-Location (Join-Path $repoRoot "go-harness")
try {
    $env:GOOS = "linux"
    $env:GOARCH = "amd64"
    $env:CGO_ENABLED = "0"
    & go build -trimpath -p=2 -o $helperBinary ./cmd/deerflow-wsl-helper
    if ($LASTEXITCODE -ne 0) { throw "Go WSL helper cross-compilation failed" }
    $protocolDeclaration = @(Select-String -LiteralPath (Join-Path $repoRoot "go-harness\internal\wslhelper\protocol.go") -Pattern '^const Version = "([a-z0-9-]+)"$')
    if ($protocolDeclaration.Count -ne 1) { throw "Cannot determine Go WSL helper protocol version" }
    $protocol = $protocolDeclaration[0].Matches[0].Groups[1].Value
    $helperDigest = (Get-FileHash -LiteralPath $helperBinary -Algorithm SHA256).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText("$helperBinary.sha256", $helperDigest + [Environment]::NewLine, [Text.UTF8Encoding]::new($false))
    @{ protocol = $protocol; platform = "linux/amd64"; executable = [IO.Path]::GetFileName($helperBinary); sha256 = $helperDigest } |
        ConvertTo-Json | Set-Content -LiteralPath "$helperBinary.json" -Encoding utf8
} finally {
    $env:GOOS = $helperPreviousGoos
    $env:GOARCH = $helperPreviousGoarch
    $env:CGO_ENABLED = $helperPreviousCgo
    Pop-Location
}
Write-Host "Go WSL helper: $helperBinary"
