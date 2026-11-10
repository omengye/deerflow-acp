# Shared by maintenance commands; matches the desktop's runtime-data.json binding.
function Get-DeerFlowDataDirectory([string]$UserData) {
    $root = [IO.Path]::GetFullPath($UserData).TrimEnd('\', '/')
    $bindingPath = Join-Path $root 'config\runtime-data.json'
    if (-not (Test-Path -LiteralPath $bindingPath)) {
        if ((Test-Path -LiteralPath (Join-Path $root 'config\go-harness-state\harness.db') -PathType Leaf) -or (Test-Path -LiteralPath (Join-Path $root 'data\go-harness\harness.db') -PathType Leaf)) {
            throw 'runtime-data.json is missing while a historical database exists; refusing to choose another store'
        }
        return Join-Path $root 'config\go-harness-state'
    }
    $file = Get-Item -LiteralPath $bindingPath
    if ($file.PSIsContainer -or $file.Length -gt 8192) { throw 'Invalid DeerFlow data directory binding' }
    $binding = Get-Content -LiteralPath $bindingPath -Raw | ConvertFrom-Json -AsHashtable
    if ($binding -isnot [hashtable] -or $binding.Count -ne 1 -or -not $binding.ContainsKey('data_dir') -or $binding.data_dir -isnot [string]) {
        throw 'Invalid DeerFlow data directory binding'
    }
    $relative = $binding.data_dir
    if ([string]::IsNullOrWhiteSpace($relative) -or [IO.Path]::IsPathRooted($relative) -or $relative -match '(^|[\\/])\.\.($|[\\/])' -or $relative.Contains(':')) {
        throw 'DeerFlow data directory must be relative to user-data'
    }
    $selected = [IO.Path]::GetFullPath((Join-Path $root $relative))
    if (-not $selected.StartsWith($root + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) { throw 'DeerFlow data directory escaped user-data' }
    if (-not (Test-Path -LiteralPath (Join-Path $selected 'harness.db') -PathType Leaf)) {
        throw "Bound DeerFlow database is missing; refusing to open another store: $selected"
    }
    # Maintenance commands must not follow a junction to a different profile.
    $current = Get-Item -LiteralPath $selected
    while ($current.FullName -ine $root) {
        if (($current.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'Bound DeerFlow data directory contains a reparse point' }
        $current = $current.Parent
    }
    foreach ($relativeStore in @('config\go-harness-state', 'data\go-harness')) {
        $other = [IO.Path]::GetFullPath((Join-Path $root $relativeStore))
        if ($other -ine $selected -and (Test-Path -LiteralPath (Join-Path $other 'harness.db') -PathType Leaf)) { throw 'A second runtime database exists; archive the inactive store outside runtime paths before starting this profile, then use the archive for skill recovery' }
    }
    return $selected
}

function Assert-DeerFlowDataDirectory($Status, [string]$Expected) {
    $actual = $Status.runtime.data_dir
    if (-not $Status.ok -or [string]::IsNullOrWhiteSpace($actual) -or [IO.Path]::GetFullPath($actual).TrimEnd('\', '/') -ine [IO.Path]::GetFullPath($Expected).TrimEnd('\', '/')) {
        throw "DeerFlow data directory mismatch: running '$actual', expected '$Expected'. Refusing to switch historical databases."
    }
}
