# Pure Git/PowerShell source identity checks shared by packaging and its fixture.
# Loading this file does not build, archive, change Git state or write files.

function Invoke-DesktopSourceGit {
    param([string]$RepositoryRoot, [string[]]$Arguments)
    $gitExecutable = @(Get-Command git -CommandType Application -ErrorAction Stop)[0].Source
    $sourceErrorPreference = $ErrorActionPreference
    try {
        # Windows PowerShell 5.1 turns redirected native stderr into error
        # records. Capture those diagnostics before testing the native exit
        # code, rather than letting Stop bypass the useful wrapper message.
        $ErrorActionPreference = 'Continue'
        $gitOutput = @(& $gitExecutable --no-optional-locks -C $RepositoryRoot @Arguments 2>&1)
        $gitExitCode = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $sourceErrorPreference
    }
    if ($gitExitCode -ne 0) {
        throw "Cannot verify desktop source with git $($Arguments -join ' '): $($gitOutput -join [Environment]::NewLine)"
    }
    foreach ($gitLine in $gitOutput) { $gitLine.ToString() }
}

function Get-DesktopSourceIdentity {
    param([Parameter(Mandatory = $true)][string]$RepositoryRoot, [switch]$RequireClean)
    $identityRoot = (Resolve-Path -LiteralPath $RepositoryRoot -ErrorAction Stop).Path
    $gitRootLines = @(Invoke-DesktopSourceGit $identityRoot @('rev-parse', '--show-toplevel'))
    $pathComparison = [StringComparison]::Ordinal
    if ([IO.Path]::DirectorySeparatorChar -eq '\') { $pathComparison = [StringComparison]::OrdinalIgnoreCase }
    if ($gitRootLines.Count -ne 1 -or -not [string]::Equals([IO.Path]::GetFullPath($gitRootLines[0]), $identityRoot, $pathComparison)) {
        throw 'Desktop source identity must be checked at the Git worktree root.'
    }
    $commitBefore = @(Invoke-DesktopSourceGit $identityRoot @('rev-parse', '--verify', 'HEAD^{commit}'))
    if ($commitBefore.Count -ne 1 -or $commitBefore[0] -cnotmatch '^([a-f0-9]{40}|[a-f0-9]{64})$') {
        throw 'Desktop source has no verifiable commit.'
    }
    $sourceStatus = @(Invoke-DesktopSourceGit $identityRoot @('status', '--porcelain=v1', '--untracked-files=all', '--ignore-submodules=none'))
    $commitAfter = @(Invoke-DesktopSourceGit $identityRoot @('rev-parse', '--verify', 'HEAD^{commit}'))
    if ($commitAfter.Count -ne 1 -or $commitAfter[0] -cne $commitBefore[0]) {
        throw 'Desktop source revision changed while checking it. Stop other source changes and retry the build.'
    }
    if ($RequireClean -and $sourceStatus.Count -ne 0) {
        $statusPreview = ($sourceStatus | Select-Object -First 8) -join [Environment]::NewLine
        throw "-CreateZip requires clean committed source, including non-ignored untracked files. Commit the intended changes or use a separate clean checkout, then rebuild. For an uncommitted development build, omit -CreateZip. Pending changes:`n$statusPreview"
    }
    [pscustomobject]@{
        RepositoryRoot = $identityRoot
        Commit = $commitBefore[0]
        Clean = ($sourceStatus.Count -eq 0)
    }
}

function Assert-DesktopSourceIdentity {
    param([Parameter(Mandatory = $true)]$Expected)
    if (-not $Expected.Clean) { throw 'A dirty development source cannot identify a corresponding source archive.' }
    $observed = Get-DesktopSourceIdentity -RepositoryRoot $Expected.RepositoryRoot
    if ($observed.Commit -cne $Expected.Commit) {
        throw "Desktop source revision changed after build preflight ($($Expected.Commit) -> $($observed.Commit)). Rebuild from one clean commit before creating ZIPs."
    }
    if (-not $observed.Clean) {
        throw 'Desktop source changed after build preflight, including tracked or non-ignored untracked files. Commit the intended changes and rebuild before creating ZIPs.'
    }
}

function Get-DesktopBuildSourceRecord {
    param(
        [Parameter(Mandatory = $true)]$Before,
        [Parameter(Mandatory = $true)]$After,
        [switch]$SkipBuild,
        [switch]$CreateZip
    )
    $sourceState = 'clean-rebuilt'
    if ($SkipBuild) {
        $sourceState = 'unverified-existing-binaries'
    } elseif ($Before.Commit -cne $After.Commit) {
        $sourceState = 'development-source-changed'
    } elseif (-not $Before.Clean -or -not $After.Clean) {
        $sourceState = 'development-dirty'
    }
    if ($CreateZip -and $sourceState -cne 'clean-rebuilt') {
        throw 'Corresponding source ZIPs require binaries rebuilt from one unchanged clean commit; omit -SkipBuild and rebuild.'
    }
    $commitLabel = $Before.Commit
    if ($sourceState -cne 'clean-rebuilt') { $commitLabel += " ($sourceState; not an exact source identity)" }
    [ordered]@{
        schema_version = 1
        source_state = $sourceState
        build_commit_label = $commitLabel
        preflight_commit = $Before.Commit
        observed_commit = $After.Commit
        rebuilt = (-not $SkipBuild)
        source_archive_commit = $(if ($CreateZip) { $Before.Commit } else { $null })
        # This gate proves commit identity only. Legacy Python source removal
        # and clean-environment release acceptance remain separate work.
        python_free_source_acceptance = 'not_verified'
    }
}

function Export-DesktopSourceArchive {
    param(
        [Parameter(Mandatory = $true)]$Identity,
        [Parameter(Mandatory = $true)][string]$Destination
    )
    $archivePath = [IO.Path]::GetFullPath($Destination)
    if (Test-Path -LiteralPath $archivePath) { throw "Source ZIP already exists: $archivePath" }
    Assert-DesktopSourceIdentity -Expected $Identity
    try {
        # HEAD is intentionally not consulted by archive: the preflight commit
        # is also the one written to the binary package's BUILD_COMMIT.txt.
        $null = Invoke-DesktopSourceGit $Identity.RepositoryRoot @('archive', '--format=zip', "--output=$archivePath", $Identity.Commit)
        Assert-DesktopSourceIdentity -Expected $Identity
    } catch {
        # Only the new file owned by this call is removed; no directory or
        # pre-existing output is ever cleaned or overwritten.
        if (Test-Path -LiteralPath $archivePath -PathType Leaf) { Remove-Item -LiteralPath $archivePath -Force }
        throw
    }
}
