[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Package,
    [ValidateRange(1, 100)][int]$Runs = 3,
    [string]$OutputDirectory
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$packageRoot = (Resolve-Path -LiteralPath $Package).Path
$tool = Join-Path $packageRoot 'deerflow-config-go.exe'
$resources = Join-Path $packageRoot 'resources'
if (-not (Test-Path -LiteralPath $tool -PathType Leaf)) { throw 'The desktop package must contain deerflow-config-go.exe at its root.' }
if (-not (Test-Path -LiteralPath (Join-Path $resources 'default-config.yaml') -PathType Leaf)) { throw 'The package must contain resources/default-config.yaml.' }
$repoRoot = Split-Path -Parent $PSScriptRoot
if ([string]::IsNullOrWhiteSpace($OutputDirectory)) { $OutputDirectory = Join-Path $repoRoot '.build-cache\settings-benchmarks' }
$output = Join-Path ([IO.Path]::GetFullPath($OutputDirectory)) ('run-' + [Guid]::NewGuid().ToString('N'))
[IO.Directory]::CreateDirectory($output) | Out-Null

function Quote-NativeArgument([string]$Value) {
    # ProcessStartInfo.Arguments uses Windows command-line quoting in PS 5.1.
    $escaped = [regex]::Replace($Value, '(\\*)"', '$1$1\"')
    $escaped = [regex]::Replace($escaped, '(\\+)$', '$1$1')
    return '"' + $escaped + '"'
}

function Invoke-Configuration([string]$UserData, [string]$Operation) {
    $arguments = @('--config', (Join-Path $UserData 'config\config.yaml'), '--user-data', $UserData, '--resources', $resources, $Operation)
    $start = New-Object Diagnostics.ProcessStartInfo
    $start.FileName = $tool
    $start.Arguments = (($arguments | ForEach-Object { Quote-NativeArgument $_ }) -join ' ')
    $start.WorkingDirectory = $output
    $start.UseShellExecute = $false
    $start.CreateNoWindow = $true
    $start.RedirectStandardInput = $true
    $start.RedirectStandardOutput = $true
    $start.RedirectStandardError = $true
    $start.StandardOutputEncoding = New-Object Text.UTF8Encoding($false)
    $start.StandardErrorEncoding = New-Object Text.UTF8Encoding($false)
    $start.EnvironmentVariables.Clear()
    foreach ($name in @('SYSTEMROOT', 'WINDIR', 'COMSPEC', 'PATH', 'TEMP', 'TMP')) {
        $value = [Environment]::GetEnvironmentVariable($name)
        if ($null -ne $value) { $start.EnvironmentVariables[$name] = $value }
    }
    $process = New-Object Diagnostics.Process
    $process.StartInfo = $start
    $watch = [Diagnostics.Stopwatch]::StartNew()
    try {
        if (-not $process.Start()) { throw 'Configuration process did not start.' }
        $stdout = $process.StandardOutput.ReadToEndAsync()
        $stderr = $process.StandardError.ReadToEndAsync()
        $process.StandardInput.Write('null')
        $process.StandardInput.Close()
        if (-not $process.WaitForExit(120000)) {
            $process.Kill()
            $process.WaitForExit()
            throw 'Configuration benchmark timed out.'
        }
        $watch.Stop()
        $envelope = $stdout.GetAwaiter().GetResult() | ConvertFrom-Json
        $null = $stderr.GetAwaiter().GetResult()
        if ($process.ExitCode -ne 0 -or -not $envelope.ok) { throw "Configuration operation failed: $Operation" }
        if ($Operation -eq 'snapshot') {
            if (-not $envelope.data.config_schema -or -not $envelope.data.config_revision) { throw 'Snapshot omitted schema or revision.' }
            if ([IO.Path]::GetFullPath($envelope.data.paths.user_data) -cne [IO.Path]::GetFullPath($UserData)) { throw 'Snapshot escaped the isolated user-data directory.' }
        }
        return [Math]::Round($watch.Elapsed.TotalMilliseconds, 1)
    } finally {
        $process.Dispose()
    }
}

$results = @()
for ($index = 0; $index -lt $Runs; $index++) {
    $userData = Join-Path $output ($index.ToString() + '\user-data')
    $timings = [ordered]@{ run = $index + 1 }
    $timings.init_ms = Invoke-Configuration $userData 'init'
    $timings.snapshot_ms = Invoke-Configuration $userData 'snapshot'
    $timings.reload_ms = Invoke-Configuration $userData 'snapshot'
    $timings.first_open_ms = [Math]::Round($timings.init_ms + $timings.snapshot_ms, 1)
    $results += [pscustomobject]$timings
    $timings | ConvertTo-Json -Compress | Write-Output
}
$median = [ordered]@{}
foreach ($name in @('init_ms', 'snapshot_ms', 'reload_ms', 'first_open_ms')) {
    $sorted = @($results | ForEach-Object { $_.$name } | Sort-Object)
    $middle = [int][Math]::Floor($sorted.Count / 2)
    if ($sorted.Count % 2) { $median[$name] = $sorted[$middle] }
    else { $median[$name] = ($sorted[$middle - 1] + $sorted[$middle]) / 2 }
}
$reportPath = Join-Path $output 'timings.json'
$report = [ordered]@{ package = $packageRoot; runs = $results; median_ms = $median }
[IO.File]::WriteAllText($reportPath, ($report | ConvertTo-Json -Depth 6), (New-Object Text.UTF8Encoding($false)))
[ordered]@{ report = $reportPath; median_ms = $median } | ConvertTo-Json -Depth 4
