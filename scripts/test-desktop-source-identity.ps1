[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
. (Join-Path $PSScriptRoot 'desktop-source-identity.ps1')

$script:identityFixtureChecks = 0
function Assert-SourceFixture([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw "Source identity fixture: $Message" }
    $script:identityFixtureChecks++
}
function Assert-SourceFixtureFailure([scriptblock]$Operation, [string]$Expected) {
    $failure = $null
    try { & $Operation | Out-Null } catch { $failure = $_.Exception.Message }
    if (-not $failure -or $failure -notlike "*$Expected*") {
        throw "Source identity fixture expected '$Expected'; observed '$failure'"
    }
    $script:identityFixtureChecks++
}

# These functions also guard the isolated build-script preflight checks. An
# unexpected fall-through cannot invoke either real compiler on the host.
function cargo { throw 'Fixture must not invoke Cargo.' }
function go { throw 'Fixture must not invoke Go.' }

$fixtureTempRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd([IO.Path]::DirectorySeparatorChar)
$fixtureRoot = Join-Path $fixtureTempRoot ("deerflow-source-identity-" + [Guid]::NewGuid().ToString('N'))
$fixtureRepository = Join-Path $fixtureRoot 'repo [fixture]'
$fixtureScripts = Join-Path $fixtureRepository 'scripts'
$fixtureArtifacts = Join-Path $fixtureRoot 'artifacts'
if (Test-Path -LiteralPath $fixtureRoot) { throw "Fixture path already exists: $fixtureRoot" }
$null = [IO.Directory]::CreateDirectory($fixtureScripts)
$null = [IO.Directory]::CreateDirectory($fixtureArtifacts)
$sourceFile = Join-Path $fixtureRepository 'source.txt'
$fixtureBuilder = Join-Path $fixtureScripts 'build-deerflow-desktop-go.ps1'
try {
    Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'desktop-source-identity.ps1') -Destination $fixtureScripts
    Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'build-deerflow-desktop-go.ps1') -Destination $fixtureScripts
    [IO.File]::WriteAllText((Join-Path $fixtureRepository '.gitignore'), "ignored/`ndist/`n")
    [IO.File]::WriteAllText($sourceFile, "version one`n")
    $null = Invoke-DesktopSourceGit $fixtureRepository @('init', '--quiet')
    $null = Invoke-DesktopSourceGit $fixtureRepository @('-c', 'core.autocrlf=false', 'add', '--', '.gitignore', 'source.txt', 'scripts')
    $null = Invoke-DesktopSourceGit $fixtureRepository @('-c', 'user.name=Source identity fixture', '-c', 'user.email=fixture@example.invalid', '-c', 'commit.gpgsign=false', '-c', 'core.hooksPath=nonexistent-fixture-hooks', 'commit', '--quiet', '-m', 'fixture source one')
    $clean = Get-DesktopSourceIdentity -RepositoryRoot $fixtureRepository -RequireClean
    Assert-SourceFixture $clean.Clean 'fresh committed source must be clean'
    Assert-SourceFixtureFailure { Invoke-DesktopSourceGit $fixtureRepository @('rev-parse', '--verify', 'refs/heads/nonexistent-source-fixture') } 'Cannot verify desktop source with git'
    if ([IO.Path]::DirectorySeparatorChar -eq '\') {
        $caseVariant = Get-DesktopSourceIdentity -RepositoryRoot $fixtureRepository.ToUpperInvariant() -RequireClean
        Assert-SourceFixture ($caseVariant.Commit -ceq $clean.Commit) 'Windows path casing must not change source identity'
    }
    Assert-DesktopSourceIdentity -Expected $clean
    $cleanRecord = Get-DesktopBuildSourceRecord -Before $clean -After $clean -CreateZip
    Assert-SourceFixture ($cleanRecord.source_state -ceq 'clean-rebuilt' -and $cleanRecord.build_commit_label -ceq $clean.Commit -and $cleanRecord.source_archive_commit -ceq $clean.Commit) 'archive and BUILD_COMMIT must use the pinned commit'
    Assert-SourceFixture ($cleanRecord.python_free_source_acceptance -ceq 'not_verified') 'identity checks must not certify the separate Python-removal gate'

    $archive = Join-Path $fixtureArtifacts 'clean-source.zip'
    Export-DesktopSourceArchive -Identity $clean -Destination $archive
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $zip = [IO.Compression.ZipFile]::OpenRead($archive)
    try {
        $entry = $zip.GetEntry('source.txt')
        Assert-SourceFixture ($null -ne $entry) 'source archive must contain committed source'
        $reader = [IO.StreamReader]::new($entry.Open())
        try { $archivedText = $reader.ReadToEnd() } finally { $reader.Dispose() }
        Assert-SourceFixture ($archivedText.TrimEnd("`r", "`n") -ceq 'version one') 'archive must retain the pinned source content'
    } finally { $zip.Dispose() }
    $zipBytes = [IO.File]::ReadAllBytes($archive)
    $zipCommit = [Text.Encoding]::ASCII.GetString($zipBytes, $zipBytes.Length - $clean.Commit.Length, $clean.Commit.Length)
    Assert-SourceFixture ($zipCommit -ceq $clean.Commit) 'Git ZIP commit comment must match BUILD_COMMIT'
    Assert-SourceFixtureFailure { Export-DesktopSourceArchive -Identity $clean -Destination $archive } 'already exists'

    $ignoredRoot = Join-Path $fixtureRepository 'ignored'
    $null = [IO.Directory]::CreateDirectory($ignoredRoot)
    [IO.File]::WriteAllText((Join-Path $ignoredRoot 'build-output.txt'), 'ignored output')
    Assert-SourceFixture (Get-DesktopSourceIdentity -RepositoryRoot $fixtureRepository -RequireClean).Clean 'ignored build output must not make the source dirty'

    [IO.File]::WriteAllText($sourceFile, "uncommitted change`n")
    Assert-SourceFixtureFailure { Get-DesktopSourceIdentity -RepositoryRoot $fixtureRepository -RequireClean } 'clean committed source'
    Assert-SourceFixtureFailure { Assert-DesktopSourceIdentity -Expected $clean } 'source changed after build preflight'
    Assert-SourceFixtureFailure { & $fixtureBuilder -CreateZip } 'clean committed source'
    Assert-SourceFixture (-not (Test-Path -LiteralPath (Join-Path $fixtureRepository 'dist'))) 'dirty preflight must not create package output'
    $dirty = Get-DesktopSourceIdentity -RepositoryRoot $fixtureRepository
    $dirtyRecord = Get-DesktopBuildSourceRecord -Before $dirty -After $dirty
    Assert-SourceFixture ($dirtyRecord.source_state -ceq 'development-dirty' -and $dirtyRecord.build_commit_label -like '*not an exact source identity*' -and $null -eq $dirtyRecord.source_archive_commit) 'development changes must remain allowed and explicitly unverified'
    Assert-SourceFixtureFailure { Get-DesktopBuildSourceRecord -Before $dirty -After $dirty -CreateZip } 'unchanged clean commit'
    $null = Invoke-DesktopSourceGit $fixtureRepository @('-c', 'core.autocrlf=false', 'add', '--', 'source.txt')
    Assert-SourceFixtureFailure { Get-DesktopSourceIdentity -RepositoryRoot $fixtureRepository -RequireClean } 'clean committed source'
    $null = Invoke-DesktopSourceGit $fixtureRepository @('restore', '--source=HEAD', '--staged', '--worktree', '--', 'source.txt')

    $untracked = Join-Path $fixtureRepository 'untracked [source].txt'
    [IO.File]::WriteAllText($untracked, 'not committed')
    Assert-SourceFixtureFailure { Get-DesktopSourceIdentity -RepositoryRoot $fixtureRepository -RequireClean } 'non-ignored untracked files'
    Assert-SourceFixtureFailure { Assert-DesktopSourceIdentity -Expected $clean } 'source changed after build preflight'
    Assert-SourceFixtureFailure { & $fixtureBuilder -CreateZip } 'non-ignored untracked files'
    Remove-Item -LiteralPath $untracked -Force
    Assert-DesktopSourceIdentity -Expected $clean

    $skippedRecord = Get-DesktopBuildSourceRecord -Before $clean -After $clean -SkipBuild
    Assert-SourceFixture ($skippedRecord.source_state -ceq 'unverified-existing-binaries' -and $skippedRecord.build_commit_label -like '*not an exact source identity*') 'existing binaries must not claim the current clean commit as their exact source'
    Assert-SourceFixtureFailure { Get-DesktopBuildSourceRecord -Before $clean -After $clean -SkipBuild -CreateZip } 'omit -SkipBuild'
    Assert-SourceFixtureFailure { & $fixtureBuilder -SkipBuild -CreateZip } 'existing binaries have no verified source identity'

    [IO.File]::WriteAllText($sourceFile, "version two`n")
    $null = Invoke-DesktopSourceGit $fixtureRepository @('-c', 'core.autocrlf=false', 'add', '--', 'source.txt')
    $null = Invoke-DesktopSourceGit $fixtureRepository @('-c', 'user.name=Source identity fixture', '-c', 'user.email=fixture@example.invalid', '-c', 'commit.gpgsign=false', '-c', 'core.hooksPath=nonexistent-fixture-hooks', 'commit', '--quiet', '-m', 'fixture source two')
    $changed = Get-DesktopSourceIdentity -RepositoryRoot $fixtureRepository -RequireClean
    Assert-SourceFixture ($changed.Commit -cne $clean.Commit) 'fixture must advance to another clean revision'
    Assert-SourceFixtureFailure { Assert-DesktopSourceIdentity -Expected $clean } 'revision changed after build preflight'
    $changedRecord = Get-DesktopBuildSourceRecord -Before $clean -After $changed
    Assert-SourceFixture ($changedRecord.source_state -ceq 'development-source-changed' -and $changedRecord.build_commit_label -like '*not an exact source identity*') 'development revision changes must not claim an exact source identity'
    $rejectedArchive = Join-Path $fixtureArtifacts 'rejected-source.zip'
    Assert-SourceFixtureFailure { Export-DesktopSourceArchive -Identity $clean -Destination $rejectedArchive } 'revision changed after build preflight'
    Assert-SourceFixture (-not (Test-Path -LiteralPath $rejectedArchive)) 'changed revision must not produce a source archive'
    Write-Output "Passed $script:identityFixtureChecks desktop source identity fixture checks. No project binaries were built or packaged."
} finally {
    if (Test-Path -LiteralPath $fixtureRoot) {
        $resolvedFixture = (Resolve-Path -LiteralPath $fixtureRoot).Path
        $resolvedTemp = (Resolve-Path -LiteralPath $fixtureTempRoot).Path.TrimEnd([IO.Path]::DirectorySeparatorChar)
        if (-not $resolvedFixture.StartsWith($resolvedTemp + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase) -or
            [IO.Path]::GetDirectoryName($resolvedFixture) -ine $resolvedTemp -or
            [IO.Path]::GetFileName($resolvedFixture) -cnotmatch '^deerflow-source-identity-[a-f0-9]{32}$') {
            throw "Refusing fixture cleanup outside its verified temporary root: $resolvedFixture"
        }
        Remove-Item -LiteralPath $resolvedFixture -Recurse -Force
    }
}
