[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$DesktopPath,
    [string]$ServerAddress = 'ws://192.168.1.190:18787',
    [Security.SecureString]$Token
)

$ErrorActionPreference = 'Stop'
$desktop = (Resolve-Path -LiteralPath $DesktopPath).Path
if (-not (Test-Path -LiteralPath $desktop -PathType Leaf)) { throw "Desktop executable was not found: $DesktopPath" }
$uri = [Uri]$ServerAddress
if ($uri.Scheme -notin @('ws', 'wss') -or -not $uri.Host -or $uri.UserInfo -or $uri.Query -or $uri.Fragment -or $uri.AbsolutePath -notin @('', '/', '/v1', '/v1/')) {
    throw 'Use a ws:// or wss:// server address without credentials, query parameters, or a custom path.'
}
$uriBuilder = [UriBuilder]::new($uri)
$uriBuilder.Path = '/v1'
$processName = [IO.Path]::GetFileNameWithoutExtension($desktop)
if (Get-Process -Name $processName -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $desktop }) { throw 'Close the existing desktop app before switching servers.' }
if (-not $Token) { $Token = Read-Host 'Server Token' -AsSecureString }
Add-Type -AssemblyName System.Security
$secretPointer = [IntPtr]::Zero
$plainToken = $null
$tokenBytes = $null
try {
    $secretPointer = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($Token)
    $plainToken = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($secretPointer)
    if ([string]::IsNullOrWhiteSpace($plainToken) -or $plainToken -match '\s') { throw 'Token is missing or contains whitespace.' }
    $tokenBytes = [Text.Encoding]::UTF8.GetBytes($plainToken)
    $encrypted = [Security.Cryptography.ProtectedData]::Protect($tokenBytes, $null, [Security.Cryptography.DataProtectionScope]::CurrentUser)
    $profileRoot = [IO.Path]::GetDirectoryName($desktop)
    $directory = Join-Path $profileRoot 'user-data/desktop'
    New-Item -ItemType Directory -Path $directory -Force > $null
    $config = [ordered]@{ remote=$true; address=$uriBuilder.Uri.AbsoluteUri; credential=@{ protection='windows-dpapi'; value=[Convert]::ToBase64String($encrypted) } }
    $configPath = Join-Path $directory 'server-connection.json'
    $temporary = Join-Path $directory ('.server-connection-' + [Guid]::NewGuid().ToString('N') + '.tmp')
    [IO.File]::WriteAllText($temporary, ($config | ConvertTo-Json -Depth 4), [Text.UTF8Encoding]::new($false))
    Move-Item -LiteralPath $temporary -Destination $configPath -Force
    $start = [Diagnostics.ProcessStartInfo]::new()
    $start.FileName = $desktop
    $start.WorkingDirectory = $profileRoot
    $start.UseShellExecute = $false
    $start.EnvironmentVariables['DEER_FLOW_PORTABLE_ROOT'] = $profileRoot
    $start.EnvironmentVariables.Remove('WAKU_DAEMON_ADDRESS')
    $start.EnvironmentVariables.Remove('WAKU_DAEMON_TOKEN')
    $process = [Diagnostics.Process]::Start($start)
    Write-Host "Desktop started with server $($uriBuilder.Uri.AbsoluteUri) (PID $($process.Id))."
} finally {
    if ($secretPointer -ne [IntPtr]::Zero) { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($secretPointer) }
    if ($tokenBytes) { [Array]::Clear($tokenBytes, 0, $tokenBytes.Length) }
    $plainToken = $null
    $tokenBytes = $null
    $Token = $null
}
