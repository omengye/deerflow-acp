[CmdletBinding()]
param([switch]$Compile, [switch]$Install, [string]$InstallerCompiler = '')

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$repo = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '..')).Path
. (Join-Path $PSScriptRoot 'windows-desktop-installer.ps1')
$cache = [IO.Path]::GetFullPath((Join-Path $repo '.build-cache'))
$fixture = Join-Path $cache ('installer-test-' + [Guid]::NewGuid().ToString('N'))
$package = Join-Path $fixture 'package with spaces'
$null = New-Item -ItemType Directory -Path $package -Force
$checks = 0
$installedUninstaller = $null
function Assert-Installer([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw "Installer fixture: $Message" }
    $script:checks++
}
function Assert-InstallerFailure([scriptblock]$Operation, [string]$Expected) {
    $message = ''
    try { & $Operation | Out-Null } catch { $message = $_.Exception.Message }
    Assert-Installer ($message -like "*$Expected*") "expected '$Expected', observed '$message'"
}
function Write-FixtureManifest {
    $lines = foreach ($file in Get-ChildItem -LiteralPath $package -Recurse -File -Force | Sort-Object FullName) {
        if ($file.Name -eq 'PACKAGE_FILES.sha256') { continue }
        $relative = $file.FullName.Substring($package.Length + 1).Replace('\', '/')
        (Get-FileHash -LiteralPath $file.FullName).Hash.ToLowerInvariant() + '  ' + $relative
    }
    $lines | Set-Content -LiteralPath (Join-Path $package 'PACKAGE_FILES.sha256') -Encoding utf8
}
try {
    foreach ($relative in @(
        'deerflow-desktop.exe', 'waku-daemon.exe', 'deerflow-acp.exe', 'deerflow-acpd.exe',
        'deerflow-config-go.exe', 'deerflow-acp-go.exe', 'deerflow-skill-go.exe',
        'deerflow-control-go.exe', 'deerflow-channel-wechat.exe',
        'deerflow-wsl-helper-linux-amd64', 'deerflow-wsl-helper-linux-amd64.sha256',
        'deerflow-wsl-helper-linux-amd64.json', 'README.txt', 'FORK.md', 'BUILD_COMMIT.txt',
        'resources\default-config.yaml', 'resources\licenses\DeerFlow.txt',
        'resources\skills\fixture\SKILL.md'
    )) {
        $path = Join-Path $package $relative
        $null = New-Item -ItemType Directory -Path ([IO.Path]::GetDirectoryName($path)) -Force
        Set-Content -LiteralPath $path -Value "Fixture: $relative" -Encoding utf8
    }
    Copy-Item -LiteralPath (Join-Path $repo 'desktop-app\LICENSE') -Destination (Join-Path $package 'resources\licenses\Waku-GPL-3.0.txt')
    $commit = 'a' * 40
    $source = [ordered]@{ rebuilt = $true; source_state = 'clean-rebuilt'; source_archive_commit = $commit; preflight_commit = $commit; observed_commit = $commit }
    $sourcePath = Join-Path $package 'BUILD_SOURCE.json'
    $source | ConvertTo-Json | Set-Content -LiteralPath $sourcePath -Encoding utf8
    $record = [ordered]@{
        package = 'desktop'; platform = 'windows/amd64'; configuration = 'Release';
        commit = $commit; regression_tests = 'passed'; packaged_bridge_tests = 'passed'; desktop_protocol_smoke = 'passed_both_modes'
    }
    $recordPath = Join-Path $package 'BUILD_PACKAGE.json'
    $record | ConvertTo-Json | Set-Content -LiteralPath $recordPath -Encoding utf8
    Write-FixtureManifest
    Assert-InstallerFailure { Get-DeerFlowInstallerFiles $package } 'matching source ZIP'
    Compress-Archive -LiteralPath (Join-Path $package 'FORK.md') -DestinationPath "$package-source.zip"
    ((Get-FileHash -LiteralPath "$package-source.zip").Hash.ToLowerInvariant() + '  package with spaces-source.zip') |
        Set-Content -LiteralPath "$package-source.zip.sha256" -Encoding ascii
    $files = @(Get-DeerFlowInstallerFiles $package)
    Assert-Installer ($files.Count -eq 22) 'complete input manifest including resources and licenses'
    Set-Content -LiteralPath "$package-source.zip.sha256" -Value 'wrong'
    Assert-InstallerFailure { Get-DeerFlowInstallerFiles $package } 'source ZIP checksum mismatch'
    ((Get-FileHash -LiteralPath "$package-source.zip").Hash.ToLowerInvariant() + '  package with spaces-source.zip') |
        Set-Content -LiteralPath "$package-source.zip.sha256" -Encoding ascii

    foreach ($relative in @('user-data\config\config.yaml', 'resources\.env', 'resources\secret.pfx', 'unexpected.exe')) {
        $path = Join-Path $package $relative
        $null = New-Item -ItemType Directory -Path ([IO.Path]::GetDirectoryName($path)) -Force
        Set-Content -LiteralPath $path -Value 'must never be packaged'
        $expected = if ($relative -eq 'unexpected.exe') { 'Unexpected installer file' } else { 'Private or runtime data' }
        Assert-InstallerFailure { Get-DeerFlowInstallerFiles $package } $expected
        Remove-Item -LiteralPath $path -Force
        if ($relative.StartsWith('user-data')) {
            # Only empty fixture directories are removed, never a real profile.
            Remove-Item -LiteralPath (Join-Path $package 'user-data\config')
            Remove-Item -LiteralPath (Join-Path $package 'user-data')
        }
    }
    $binary = Join-Path $package 'deerflow-acpd.exe'
    $original = [IO.File]::ReadAllBytes($binary)
    Add-Content -LiteralPath $binary -Value 'tampered'
    Assert-InstallerFailure { Get-DeerFlowInstallerFiles $package } 'checksum mismatch'
    [IO.File]::WriteAllBytes($binary, $original)
    Remove-Item -LiteralPath $binary
    Assert-InstallerFailure { Get-DeerFlowInstallerFiles $package } 'missing or empty: deerflow-acpd.exe'
    [IO.File]::WriteAllBytes($binary, $original)
    $manifestPath = Join-Path $package 'PACKAGE_FILES.sha256'
    Add-Content -LiteralPath $manifestPath -Value (('b' * 64) + '  ../outside.txt')
    Assert-InstallerFailure { Get-DeerFlowInstallerFiles $package } 'Unsafe or duplicate'
    Write-FixtureManifest
    Add-Content -LiteralPath $manifestPath -Value (('b' * 64) + '  missing.txt')
    Assert-InstallerFailure { Get-DeerFlowInstallerFiles $package } 'missing files'
    Write-FixtureManifest
    Add-Content -LiteralPath $manifestPath -Value (Get-Content -LiteralPath $manifestPath -First 1)
    Assert-InstallerFailure { Get-DeerFlowInstallerFiles $package } 'Unsafe or duplicate'
    Write-FixtureManifest
    $record.desktop_protocol_smoke = 'failed'
    $record | ConvertTo-Json | Set-Content -LiteralPath $recordPath
    Assert-InstallerFailure { Get-DeerFlowInstallerFiles $package -Development } 'both desktop protocol smoke'
    $record.desktop_protocol_smoke = 'passed_both_modes'
    $record.regression_tests = 'skipped_development'
    $record | ConvertTo-Json | Set-Content -LiteralPath $recordPath
    Assert-InstallerFailure { Get-DeerFlowInstallerFiles $package } 'passed regression tests'
    $source.source_state = 'development-dirty'
    $source.source_archive_commit = $null
    $source | ConvertTo-Json | Set-Content -LiteralPath $sourcePath
    Write-FixtureManifest
    Assert-Installer (@(Get-DeerFlowInstallerFiles $package -Development).Count -eq $files.Count) 'explicit local development inputs accepted'
    $source.rebuilt = $false
    $source | ConvertTo-Json | Set-Content -LiteralPath $sourcePath
    Assert-InstallerFailure { Get-DeerFlowInstallerFiles $package -Development } 'requires rebuilt binaries'
    $source.rebuilt = $true
    $source.source_state = 'development-source-changed'
    $source | ConvertTo-Json | Set-Content -LiteralPath $sourcePath
    Assert-InstallerFailure { Get-DeerFlowInstallerFiles $package -Development } 'unchanged source'
    $source.source_state = 'development-dirty'
    $source | ConvertTo-Json | Set-Content -LiteralPath $sourcePath
    Write-FixtureManifest

    $template = Get-Content -LiteralPath (Join-Path $repo 'resources\windows\deerflow-desktop.iss') -Raw
    Assert-Installer ($template -match '(?m)^DefaultDirName=\{localappdata\}\\Programs\\DeerFlowDesktop\r?$') 'default installation directory uses DeerFlowDesktop without spaces'
    Assert-Installer ($template -match '(?m)^AppName=DeerFlow Desktop\r?$') 'application display name remains unchanged'
    Assert-Installer ($template -match '(?m)^UsePreviousAppDir=yes\r?$') 'upgrades retain the existing installation directory'
    Assert-Installer ($template -match 'PrivilegesRequired=lowest' -and $template -match 'skipifsilent') 'per-user and no launch on silent install'
    Assert-Installer ($template -notmatch '(?m)^SignTool\s*=') 'installer compilation does not require a signing certificate'
    Assert-Installer ($template -notmatch '(?m)^\[(InstallDelete|UninstallDelete)\]' -and $template -match 'CloseApplications=no') 'no recursive deletion or forced shutdown'
    Assert-Installer ($template -match 'CheckInstalledProcesses' -and $template -notmatch '8B6C6E4A-3E0F-4F0B-9C5F-2E0E9C4B7A11') 'busy-process guard and independent product identity'
    $entry = Join-Path $PSScriptRoot 'build-windows.ps1'
    Assert-InstallerFailure { & $entry -Package ACP -CreateInstaller } 'requires -Package Desktop or All'
    Assert-InstallerFailure { & $entry -Package Desktop -Development -Configuration Debug -CreateInstaller } 'requires -Configuration Release'

    function Invoke-FixtureCompiler {
        $outputDir = ($args | Where-Object { $_ -like '/DOutputDir=*' }).Substring(12)
        $baseName = ($args | Where-Object { $_ -like '/DOutputBaseName=*' }).Substring(17)
        $include = ($args | Where-Object { $_ -like '/DFileList=*' }).Substring(11)
        $contents = Get-Content -LiteralPath $include -Raw
        Assert-Installer ($contents -match 'deerflow-acpd.exe' -and $contents -notmatch 'user-data' -and $contents -notmatch 'recursesubdirs') 'compiler consumes explicit frozen files'
        Set-Content -LiteralPath (Join-Path $outputDir "$baseName.exe") -Value 'fixture installer, not executable'
        'Compiler fixture diagnostic output must not leak into the returned filename.'
        $global:LASTEXITCODE = 0
    }
    $parameters = @{ PackageDirectory = $package; OutputDirectory = (Join-Path $fixture 'output'); RepositoryRoot = $repo; Version = '0.1.19'; Development = $true }
    $result = New-DeerFlowDesktopInstaller @parameters -CompilerPath 'Invoke-FixtureCompiler'
    Assert-Installer ($result.EndsWith('-development-windows-x64-Setup.exe')) 'development installer cannot be mistaken for a distribution build'
    $checksum = (Get-Content -LiteralPath "$result.sha256" -Raw).Trim()
    Assert-Installer ($checksum -ceq ((Get-FileHash -LiteralPath $result).Hash.ToLowerInvariant() + '  ' + [IO.Path]::GetFileName($result))) 'installer checksum'
    Assert-InstallerFailure { New-DeerFlowDesktopInstaller @parameters -CompilerPath 'Invoke-FixtureCompiler' } 'already exists'
    $parameters.Version = '0.1.99999'
    Assert-InstallerFailure { New-DeerFlowDesktopInstaller @parameters -CompilerPath 'Invoke-FixtureCompiler' } 'at most 65535'
    $parameters.Version = '0.1.19'
    $parameters.OutputDirectory = Join-Path $package 'nested-output'
    Assert-InstallerFailure { New-DeerFlowDesktopInstaller @parameters -CompilerPath 'Invoke-FixtureCompiler' } 'outside the package'
    function Invoke-FailingCompiler { $global:LASTEXITCODE = 29 }
    $parameters.OutputDirectory = Join-Path $fixture 'failure'
    Assert-InstallerFailure { New-DeerFlowDesktopInstaller @parameters -CompilerPath 'Invoke-FailingCompiler' } 'compilation failed (exit 29)'
    Assert-Installer (@(Get-ChildItem -LiteralPath $parameters.OutputDirectory -Force).Count -eq 0) 'failed compiler left no installer or checksum'
    $source.source_state = 'clean-rebuilt'
    $source.source_archive_commit = $commit
    $source | ConvertTo-Json | Set-Content -LiteralPath $sourcePath
    $record.regression_tests = 'passed'
    $record | ConvertTo-Json | Set-Content -LiteralPath $recordPath
    Write-FixtureManifest
    $parameters.Development = $false
    $parameters.OutputDirectory = Join-Path $fixture 'distribution'
    $result = New-DeerFlowDesktopInstaller @parameters -CompilerPath 'Invoke-FixtureCompiler'
    Assert-Installer ($result.EndsWith('0.1.19-windows-x64-Setup.exe')) 'verified distribution filename'
    if ($Compile -or $Install) {
        $compiler = Find-DeerFlowInstallerCompiler -CompilerPath $InstallerCompiler
        $parameters.OutputDirectory = Join-Path $fixture 'compiled'
        if ($Install) {
            # A separate identity ensures this test never modifies a real install.
            $testRepo = Join-Path $fixture 'isolated repository'
            $testTemplate = Join-Path $testRepo 'resources\windows\deerflow-desktop.iss'
            $testIcon = Join-Path $testRepo 'desktop-app\resources\windows\AppIcon.ico'
            $null = New-Item -ItemType Directory -Path ([IO.Path]::GetDirectoryName($testTemplate)), ([IO.Path]::GetDirectoryName($testIcon)) -Force
            $testId = [Guid]::NewGuid().ToString().ToUpperInvariant()
            $testName = "DeerFlow Installer Fixture $testId"
            $template.Replace('A77F56A9-B6EF-42CC-A36D-E80592F4092B', $testId).Replace('DeerFlow Desktop', $testName).Replace('DefaultDirName={localappdata}\Programs\DeerFlowDesktop', "DefaultDirName={localappdata}\Programs\$testName") |
                Set-Content -LiteralPath $testTemplate -Encoding utf8
            Copy-Item -LiteralPath (Join-Path $repo 'desktop-app\resources\windows\AppIcon.ico') -Destination $testIcon
            $parameters.RepositoryRoot = $testRepo
        }
        $result = New-DeerFlowDesktopInstaller @parameters -CompilerPath $compiler
        $header = [IO.File]::ReadAllBytes($result)
        Assert-Installer ($header[0] -eq 0x4d -and $header[1] -eq 0x5a) 'real Inno Setup PE output'
        if ($Install) {
            $installRoot = Join-Path $fixture 'installed app with spaces'
            $installLog = Join-Path $fixture 'install.log'
            $installArguments = @('/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART', '/NOICONS', '/TASKS=', "/DIR=`"$installRoot`"", "/LOG=`"$installLog`"")
            $process = Start-Process -FilePath $result -ArgumentList $installArguments -WindowStyle Hidden -PassThru -Wait
            if ($process.ExitCode -ne 0) {
                Get-Content -LiteralPath $installLog -Tail 35 | Out-Host
                throw "Installer lifecycle fixture failed: $($process.ExitCode)"
            }
            $installedUninstaller = Join-Path $installRoot 'unins000.exe'
            Assert-Installer (Test-Path -LiteralPath $installedUninstaller -PathType Leaf) 'silent installation created its uninstaller'
            Assert-Installer (-not (Test-Path -LiteralPath (Join-Path $installRoot 'user-data'))) 'silent installation did not launch or create a profile'
            foreach ($relative in $files) {
                Assert-Installer ((Get-FileHash -LiteralPath (Join-Path $installRoot $relative)).Hash -ceq (Get-FileHash -LiteralPath (Join-Path $package $relative)).Hash) "installed bytes: $relative"
            }
            $profile = Join-Path $installRoot 'user-data\config'
            $null = New-Item -ItemType Directory -Path $profile -Force
            $sentinel = Join-Path $profile 'installation-test.txt'
            Set-Content -LiteralPath $sentinel -Value 'Preserve this isolated test profile, not a real credential.'
            $sentinelHash = (Get-FileHash -LiteralPath $sentinel).Hash
            $busyHelper = Join-Path $installRoot 'deerflow-installer-fixture.exe'
            Copy-Item -LiteralPath (Join-Path $env:SystemRoot 'System32\ping.exe') -Destination $busyHelper
            $busyProcess = Start-Process -FilePath $busyHelper -ArgumentList '-t 127.0.0.1' -WindowStyle Hidden -PassThru
            try {
                Assert-Installer (-not $busyProcess.HasExited) 'isolated busy-process helper started'
                $process = Start-Process -FilePath $result -ArgumentList $installArguments -WindowStyle Hidden -PassThru -Wait
                Assert-Installer ($process.ExitCode -ne 0 -and -not $busyProcess.HasExited) 'upgrade refuses a busy installation without killing its processes'
                Assert-Installer ((Get-Content -LiteralPath $installLog -Raw) -match 'DeerFlow is still running') 'busy-process failure reports the actual reason'
                $process = Start-Process -FilePath $installedUninstaller -ArgumentList '/VERYSILENT /SUPPRESSMSGBOXES /NORESTART' -WindowStyle Hidden -PassThru -Wait
                Assert-Installer ($process.ExitCode -ne 0 -and -not $busyProcess.HasExited) 'uninstall refuses a busy installation without killing its processes'
            } finally {
                if (-not $busyProcess.HasExited) { Stop-Process -Id $busyProcess.Id -ErrorAction Stop; $busyProcess.WaitForExit() }
                Remove-Item -LiteralPath $busyHelper -Force
            }
            $process = Start-Process -FilePath $result -ArgumentList $installArguments -WindowStyle Hidden -PassThru -Wait
            Assert-Installer ($process.ExitCode -eq 0) 'same-directory upgrade completed'
            Assert-Installer ((Get-FileHash -LiteralPath $sentinel).Hash -ceq $sentinelHash) 'upgrade preserved profile bytes'
            $process = Start-Process -FilePath $installedUninstaller -ArgumentList '/VERYSILENT /SUPPRESSMSGBOXES /NORESTART' -WindowStyle Hidden -PassThru -Wait
            Assert-Installer ($process.ExitCode -eq 0) 'uninstall completed'
            $installedUninstaller = $null
            Assert-Installer ((Get-FileHash -LiteralPath $sentinel).Hash -ceq $sentinelHash) 'uninstall preserved profile bytes'
            Assert-Installer (-not (Test-Path -LiteralPath (Join-Path $installRoot 'deerflow-desktop.exe'))) 'uninstall removed the installed desktop'
        }
    }
    Write-Host "Windows installer: $checks input, privacy, identity, failure and checksum checks passed. Compile=$($Compile -or $Install) Install=$Install"
    $global:LASTEXITCODE = 0
} finally {
    if ($installedUninstaller -and (Test-Path -LiteralPath $installedUninstaller -PathType Leaf)) {
        $cleanup = Start-Process -FilePath $installedUninstaller -ArgumentList '/VERYSILENT /SUPPRESSMSGBOXES /NORESTART' -WindowStyle Hidden -PassThru -Wait
        if ($cleanup.ExitCode -ne 0) { throw "Fixture uninstall failed; retained for inspection: $fixture" }
    }
    $checked = [IO.Path]::GetFullPath($fixture)
    if (-not $checked.StartsWith($cache + '\', [StringComparison]::OrdinalIgnoreCase)) { throw 'Installer fixture cleanup escaped build cache.' }
    Remove-Item -LiteralPath $checked -Recurse -Force
}
