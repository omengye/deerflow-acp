Set-StrictMode -Version Latest

function Assert-GoRepositorySources {
    [CmdletBinding()]
    param([Parameter(Mandatory = $true)][string]$RepositoryRoot)

    $root = (Resolve-Path -LiteralPath $RepositoryRoot -ErrorAction Stop).Path
    $paths = @(& git -C $root -c core.quotepath=false ls-files --cached --others --exclude-standard)
    if ($LASTEXITCODE -ne 0) { throw 'Cannot enumerate repository source inputs' }
    $violations = @()
    foreach ($path in ($paths | Select-Object -Unique)) {
        $absolute = [IO.Path]::GetFullPath((Join-Path $root $path))
        if (-not $absolute.StartsWith($root + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
            throw "Source path escaped repository: $path"
        }
        # git ls-files includes files deleted in the worktree until committed.
        if (-not (Test-Path -LiteralPath $absolute -PathType Leaf)) { continue }
        $name = [IO.Path]::GetFileName($absolute).ToLowerInvariant()
        $extension = [IO.Path]::GetExtension($name)
        # Skill source is optional user functionality, not a Go host dependency.
        $skillSource = $path.Replace('\', '/').StartsWith('skills/', [StringComparison]::OrdinalIgnoreCase)
        if ($skillSource -and $extension -notin @('.pyc', '.pyo', '.pyd', '.whl', '.pyz') -and
            $path.Replace('\', '/') -notmatch '(?i)(^|/)(\.venv|site-packages|__pycache__|conda-meta)(/|$)') { continue }
        if ($extension -in @('.py', '.pyw', '.pyc', '.pyo', '.pyd', '.whl', '.pyz') -or
            $name -in @('pyproject.toml', 'uv.lock', 'pipfile', 'pipfile.lock', 'poetry.lock', 'setup.py', '.python-version', 'tox.ini', 'pytest.ini') -or
            $name -match '^requirements(?:[-_.].*)?\.(?:txt|in)$' -or
            $path.Replace('\', '/') -match '(?i)(^|/)(\.venv|site-packages|__pycache__|conda-meta)(/|$)') {
            $violations += $path
        }
    }
    if ($violations.Count -gt 0) {
        throw "Python source/build dependencies remain in repository inputs: $($violations -join ', ')"
    }
    # This checks named source/dependency files. Historical prose and negative
    # test strings are allowed; it does not prove a clean release environment.
}

function Assert-GoBundledSkills {
    [CmdletBinding()]
    param([Parameter(Mandatory = $true)][string]$Source)

    $root = (Resolve-Path -LiteralPath $Source -ErrorAction Stop).Path
    $pending = New-Object 'System.Collections.Generic.Stack[string]'
    $pending.Push($root)
    while ($pending.Count -gt 0) {
        foreach ($entry in Get-ChildItem -LiteralPath $pending.Pop() -Force) {
            if (($entry.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
                throw "Bundled skill input contains a reparse point: $($entry.FullName)"
            }
            if ($entry.PSIsContainer) {
                if ($entry.Name -in @('.venv', 'site-packages', '__pycache__', 'conda-meta')) {
                    throw "Bundled skill input contains a Python runtime environment: $($entry.FullName)"
                }
                if ($entry.Name -notin @('.git', 'node_modules', '.cache', 'user-data', 'logs')) {
                    $pending.Push($entry.FullName)
                }
                continue
            }
            if ($entry.Extension -in @('.pyc', '.pyo', '.pyd', '.whl', '.pyz', '.exe', '.dll', '.so') -or $entry.Name -eq 'pyvenv.cfg') {
                throw "Bundled skill input contains a runtime or compiled artifact: $($entry.FullName)"
            }
            if ($entry.Extension -in @('.py', '.pyw', '.toml') -or $entry.Name -match '^requirements.*\.(txt|in)$') {
                $text = [Text.UTF8Encoding]::new($false, $true).GetString([IO.File]::ReadAllBytes($entry.FullName))
                if ($text.Contains([char]0)) { throw "Bundled skill source is not reviewable text: $($entry.FullName)" }
            }
        }
    }
}
