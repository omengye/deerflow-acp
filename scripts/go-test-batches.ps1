Set-StrictMode -Version Latest

function Get-GoTestInventory {
    param([string[]]$Lines)
    $seen = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    foreach ($line in $Lines) {
        if ($line -cmatch '^(Test|Example|Fuzz)\S*$') {
            if (-not $seen.Add($line)) { throw "Duplicate Go test in inventory: $line" }
            $line
        } elseif ($line -and $line -notmatch '^(ok\s|\?\s)') {
            throw "Unexpected Go test inventory output: $line"
        }
    }
}

function Get-GoTestBatches {
    param([string[]]$Names, [ValidateRange(1, 256)][int]$BatchSize = 32)
    for ($start = 0; $start -lt $Names.Count; $start += $BatchSize) {
        $end = [Math]::Min($start + $BatchSize, $Names.Count) - 1
        [pscustomobject]@{ Names = @($Names[$start..$end]) }
    }
    if ($Names.Count -eq 0) { [pscustomobject]@{ Names = @() } }
}

function Get-GoTestPattern {
    param([string[]]$Names)
    if ($Names.Count -eq 0) { return '^$' }
    return '^(' + (($Names | ForEach-Object { [regex]::Escape($_) }) -join '|') + ')$'
}

function Assert-GoTestCoverage {
    param([string[]]$Names, [Collections.IDictionary]$Results)
    foreach ($name in $Names) {
        if (-not $Results.Contains($name)) { throw "Missing Go test result: $name" }
        if ($Results[$name] -notin @('pass', 'skip')) { throw "Go test failed: $name" }
    }
    foreach ($name in $Results.Keys) {
        if ($Names -cnotcontains $name) { throw "Unexpected Go test result: $name" }
    }
}
