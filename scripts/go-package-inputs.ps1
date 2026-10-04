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
        if ($extension -in @('.py', '.pyw', '.pyc', '.pyo', '.pyd', '.whl') -or
            $name -in @('pyproject.toml', 'uv.lock', 'pipfile', 'pipfile.lock', 'poetry.lock', 'setup.py', '.python-version', 'tox.ini', 'pytest.ini') -or
            $name -match '^requirements(?:[-_.].*)?\.(?:txt|in)$') {
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
    # Match commands, including fenced/inline examples. References to a Python
    # project, docstrings, or glob patterns such as --include="*.py" are data.
    $commandPattern = '(?im)(?:^[ \t]*(?:[$>][ \t]*)?|`)(?:python(?:w|[23](?:\.\d+)*)?|pip[23]?|uv(?:x)?)[ \t]+(?!is\b|scripts?[ \t]+(?:are|were)\b|runtime\b|project\b)[^\r\n`]+'
    $scriptPattern = '(?im)(?:^\s*(?:[$>]\s*)?|`)(?:\./|\.\\|/)[^\s`]*\.pyw?(?:\s|`|$)|^#![^\r\n]*\bpython[23]?\b'
    while ($pending.Count -gt 0) {
        foreach ($entry in Get-ChildItem -LiteralPath $pending.Pop() -Force) {
            if (($entry.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
                throw "Bundled skill input contains a reparse point: $($entry.FullName)"
            }
            if ($entry.PSIsContainer) {
                if ($entry.Name -notin @('.git', '.venv', 'node_modules', '__pycache__', '.cache', 'user-data', 'logs')) {
                    $pending.Push($entry.FullName)
                }
                continue
            }
            if ($entry.Extension -in @('.py', '.pyw', '.pyc', '.pyo', '.pyd')) {
                throw "Bundled skill input requires Python: $($entry.FullName). Migrate or explicitly retire the skill in source before building."
            }
            if ($entry.Extension -in @('.md', '.sh', '.ps1', '.cmd', '.bat')) {
                $text = [IO.File]::ReadAllText($entry.FullName)
                if ($text -match $commandPattern -or $text -match $scriptPattern) {
                    throw "Bundled skill input contains a Python execution command: $($entry.FullName). Migrate the instruction before building."
                }
            }
        }
    }
}
