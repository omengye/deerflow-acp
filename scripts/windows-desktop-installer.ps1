# Shared installer preflight and packaging. Dot-sourcing does not build or install.
function Find-DeerFlowInstallerCompiler {
    param([string]$CompilerPath = '')
    if ($CompilerPath) {
        return (Resolve-Path -LiteralPath $CompilerPath -ErrorAction Stop).Path
    }
    $command = Get-Command ISCC.exe -CommandType Application -ErrorAction SilentlyContinue
    if ($command) { return $command.Source }
    foreach ($base in @($env:ProgramFiles, ${env:ProgramFiles(x86)}, $(if ($env:LOCALAPPDATA) { Join-Path $env:LOCALAPPDATA 'Programs' }))) {
        if (-not $base) { continue }
        $candidate = Join-Path $base 'Inno Setup 6\ISCC.exe'
        if (Test-Path -LiteralPath $candidate -PathType Leaf) { return $candidate }
    }
    throw 'Inno Setup 6 (ISCC.exe) is required for -CreateInstaller. Install it with winget install --id JRSoftware.InnoSetup --exact, or pass -InstallerCompiler.'
}

function Get-DeerFlowInstallerFiles {
    param([Parameter(Mandatory)][string]$PackageDirectory, [switch]$Development)
    $root = (Resolve-Path -LiteralPath $PackageDirectory).Path.TrimEnd('\')
    $required = @(
        'deerflow-desktop.exe', 'waku-daemon.exe', 'deerflow-acp.exe',
        'deerflow-acpd.exe', 'deerflow-config-go.exe', 'deerflow-acp-go.exe',
        'deerflow-skill-go.exe', 'deerflow-control-go.exe', 'deerflow-channel-wechat.exe',
        'deerflow-wsl-helper-linux-amd64', 'deerflow-wsl-helper-linux-amd64.sha256',
        'deerflow-wsl-helper-linux-amd64.json', 'README.txt', 'FORK.md',
        'BUILD_COMMIT.txt', 'BUILD_SOURCE.json', 'BUILD_PACKAGE.json', 'PACKAGE_FILES.sha256',
        'resources\default-config.yaml', 'resources\licenses\Waku-GPL-3.0.txt',
        'resources\licenses\DeerFlow.txt'
    )
    foreach ($relative in $required) {
        $file = Join-Path $root $relative
        if (-not (Test-Path -LiteralPath $file -PathType Leaf) -or (Get-Item -LiteralPath $file).Length -eq 0) {
            throw "Installer input is missing or empty: $relative"
        }
    }
    $record = Get-Content -LiteralPath (Join-Path $root 'BUILD_PACKAGE.json') -Raw | ConvertFrom-Json
    $source = Get-Content -LiteralPath (Join-Path $root 'BUILD_SOURCE.json') -Raw | ConvertFrom-Json
    if ($record.package -cne 'desktop' -or $record.platform -cne 'windows/amd64' -or $record.configuration -cne 'Release') {
        throw 'Installer requires a Release Windows x64 desktop package.'
    }
    if (-not $source.rebuilt -or $source.source_state -in @('unverified-existing-binaries', 'development-source-changed')) {
        throw 'Installer requires rebuilt binaries from unchanged source; reused binaries are not accepted.'
    }
    if ($record.packaged_bridge_tests -cne 'passed' -or $record.desktop_protocol_smoke -cne 'passed_both_modes') {
        throw 'Installer requires passed packaged Bridge tests and both desktop protocol smoke modes.'
    }
    if (-not $Development) {
        if ($record.regression_tests -cne 'passed' -or $source.source_state -cne 'clean-rebuilt' -or
            $source.source_archive_commit -cne $record.commit -or $source.preflight_commit -cne $record.commit -or
            $source.observed_commit -cne $record.commit) {
            throw 'Distribution installer requires passed regression tests and exact corresponding source identity. Use -Development for a local test installer.'
        }
        $sourceZip = "$root-source.zip"
        if (-not (Test-Path -LiteralPath $sourceZip -PathType Leaf) -or -not (Test-Path -LiteralPath "$sourceZip.sha256" -PathType Leaf)) {
            throw 'Distribution installer requires the matching source ZIP and its SHA-256 file beside the package.'
        }
        $expected = (Get-FileHash -LiteralPath $sourceZip -Algorithm SHA256).Hash.ToLowerInvariant() + '  ' + [IO.Path]::GetFileName($sourceZip)
        if ((Get-Content -LiteralPath "$sourceZip.sha256" -Raw).Trim() -cne $expected) { throw 'Corresponding source ZIP checksum mismatch.' }
    }
    $hashes = @{}
    foreach ($line in Get-Content -LiteralPath (Join-Path $root 'PACKAGE_FILES.sha256')) {
        if ($line -cnotmatch '^([a-f0-9]{64})  (.+)$') { throw 'Invalid package file manifest.' }
        $digest = $Matches[1]
        $relative = $Matches[2].Replace('/', '\')
        if ([IO.Path]::IsPathRooted($relative) -or $relative -match '(^|\\)\.\.?($|\\)|[:\r\n"]' -or $hashes.ContainsKey($relative)) {
            throw "Unsafe or duplicate package manifest path: $relative"
        }
        $hashes[$relative] = $digest
    }
    $files = [Collections.Generic.List[string]]::new()
    $pending = [Collections.Generic.Stack[string]]::new()
    $pending.Push($root)
    while ($pending.Count -gt 0) {
        $directory = $pending.Pop()
        if (((Get-Item -LiteralPath $directory -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw "Installer input contains a reparse point: $directory"
        }
        foreach ($entry in Get-ChildItem -LiteralPath $directory -Force) {
            $relative = $entry.FullName.Substring($root.Length + 1)
            if (($entry.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw "Installer input contains a reparse point: $relative" }
            # Never package a used installation, personal data, credentials or caches.
            if ($entry.Name -match '^(user-data|\.git|\.venv|node_modules|__pycache__|logs|\.cache)$|^\.env($|\.(?!example$))' -or
                $entry.Extension -in @('.key', '.pfx', '.p12', '.log', '.db', '.sqlite', '.pyc', '.pyo', '.pyd', '.whl')) {
                throw "Private or runtime data is not an installer input: $relative"
            }
            if ($entry.PSIsContainer) {
                if ($relative -ne 'resources' -and -not $relative.StartsWith('resources\', [StringComparison]::OrdinalIgnoreCase)) {
                    throw "Unexpected installer directory: $relative"
                }
                $pending.Push($entry.FullName)
                continue
            }
            if ($relative -notin $required -and -not $relative.StartsWith('resources\', [StringComparison]::OrdinalIgnoreCase)) {
                throw "Unexpected installer file: $relative"
            }
            if ($relative -match '[\r\n"]') { throw "Unsafe installer filename: $relative" }
            if ($relative -ne 'PACKAGE_FILES.sha256') {
                if (-not $hashes.ContainsKey($relative) -or (Get-FileHash -LiteralPath $entry.FullName -Algorithm SHA256).Hash.ToLowerInvariant() -cne $hashes[$relative]) {
                    throw "Installer input checksum mismatch: $relative"
                }
                $hashes.Remove($relative)
            }
            $files.Add($relative)
        }
    }
    if ($hashes.Count -ne 0) { throw 'Package manifest references missing files.' }
    return $files | Sort-Object
}

function New-DeerFlowDesktopInstaller {
    param(
        [Parameter(Mandatory)][string]$PackageDirectory,
        [Parameter(Mandatory)][string]$OutputDirectory,
        [Parameter(Mandatory)][string]$RepositoryRoot,
        [Parameter(Mandatory)][string]$CompilerPath,
        [Parameter(Mandatory)][string]$Version,
        [switch]$Development
    )
    if ($Version -notmatch '^\d+\.\d+\.\d+(\.\d+)?$' -or @($Version.Split('.') | Where-Object { [long]$_ -gt 65535 }).Count) {
        throw 'Installer version must have three or four numeric components, each at most 65535.'
    }
    $root = (Resolve-Path -LiteralPath $PackageDirectory).Path
    $files = @(Get-DeerFlowInstallerFiles -PackageDirectory $root -Development:$Development)
    $output = [IO.Path]::GetFullPath($OutputDirectory)
    if ($output -eq $root -or $output.StartsWith($root + '\', [StringComparison]::OrdinalIgnoreCase)) {
        throw 'Installer output must be outside the package directory.'
    }
    $suffix = if ($Development) { '-development' } else { '' }
    $baseName = "DeerFlow-Desktop-$Version$suffix-windows-x64-Setup"
    $installer = Join-Path $output "$baseName.exe"
    foreach ($path in @($installer, "$installer.sha256")) {
        if (Test-Path -LiteralPath $path) { throw "Installer output already exists: $path" }
    }
    $null = New-Item -ItemType Directory -Path $output -Force
    $cache = [IO.Path]::GetFullPath((Join-Path $RepositoryRoot '.build-cache'))
    $stage = Join-Path $cache ('installer-' + [Guid]::NewGuid().ToString('N'))
    $null = New-Item -ItemType Directory -Path $stage
    try {
        # Freeze only validated files; ISCC never recurses over a live installation.
        foreach ($relative in $files) {
            $destination = Join-Path $stage $relative
            $null = New-Item -ItemType Directory -Path ([IO.Path]::GetDirectoryName($destination)) -Force
            Copy-Item -LiteralPath (Join-Path $root $relative) -Destination $destination
        }
        $null = Get-DeerFlowInstallerFiles -PackageDirectory $root -Development:$Development
        foreach ($relative in $files) {
            if ((Get-FileHash -LiteralPath (Join-Path $root $relative)).Hash -cne (Get-FileHash -LiteralPath (Join-Path $stage $relative)).Hash) {
                throw "Installer input changed while staging: $relative"
            }
        }
        $include = Join-Path $stage 'installer-files.iss'
        $lines = foreach ($relative in $files) {
            $subdirectory = [IO.Path]::GetDirectoryName($relative)
            $destination = if ($subdirectory) { '{app}\' + $subdirectory.Replace('{', '{{') } else { '{app}' }
            'Source: "' + (Join-Path $stage $relative) + '"; DestDir: "' + $destination + '"; Flags: ignoreversion'
        }
        $lines | Set-Content -LiteralPath $include -Encoding utf8
        $template = Join-Path $RepositoryRoot 'resources\windows\deerflow-desktop.iss'
        $icon = Join-Path $RepositoryRoot 'desktop-app\resources\windows\AppIcon.ico'
        & $CompilerPath "/DAppVersion=$Version" "/DStageDir=$stage" "/DOutputDir=$output" "/DOutputBaseName=$baseName" "/DAppIcon=$icon" "/DFileList=$include" $template | Out-Host
        if ($LASTEXITCODE -ne 0) { throw "Inno Setup compilation failed (exit $LASTEXITCODE)." }
        if (-not (Test-Path -LiteralPath $installer -PathType Leaf) -or (Get-Item -LiteralPath $installer).Length -eq 0) {
            throw 'Inno Setup did not produce a nonempty installer.'
        }
        $digest = (Get-FileHash -LiteralPath $installer -Algorithm SHA256).Hash.ToLowerInvariant()
        "$digest  $baseName.exe" | Set-Content -LiteralPath "$installer.sha256" -Encoding ascii
        Write-Host "Unsigned desktop installer: $installer"
        return $installer
    } catch {
        foreach ($path in @($installer, "$installer.sha256")) {
            if (Test-Path -LiteralPath $path -PathType Leaf) { Remove-Item -LiteralPath $path -Force }
        }
        throw
    } finally {
        $checkedStage = [IO.Path]::GetFullPath($stage)
        if (-not $checkedStage.StartsWith($cache + '\', [StringComparison]::OrdinalIgnoreCase)) { throw 'Installer cleanup escaped build cache.' }
        Remove-Item -LiteralPath $checkedStage -Recurse -Force
    }
}
