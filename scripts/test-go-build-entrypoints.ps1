[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$repository = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '..')).Path
$cache = Join-Path $repository '.build-cache'
$fixture = Join-Path $cache ('go-entrypoints-' + [Guid]::NewGuid().ToString('N'))
$resolvedFixture = [IO.Path]::GetFullPath($fixture)
if (-not $resolvedFixture.StartsWith([IO.Path]::GetFullPath($cache) + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
    throw 'Fixture escaped build cache'
}
New-Item -ItemType Directory -Path $fixture -Force | Out-Null
function Assert-Fixture([bool]$Condition, [string]$Message) { if (-not $Condition) { throw $Message } }
try {
    foreach ($path in Get-ChildItem -LiteralPath $PSScriptRoot -Filter '*.ps1') {
        $tokens = $null
        $errors = $null
        [void][Management.Automation.Language.Parser]::ParseFile($path.FullName, [ref]$tokens, [ref]$errors)
        Assert-Fixture ($errors.Count -eq 0) "PowerShell syntax: $($path.Name): $errors"
    }
    foreach ($entry in @('desktop', 'portable')) {
        $wrapper = Join-Path $fixture "build-deerflow-$entry.ps1"
        Copy-Item -LiteralPath (Join-Path $PSScriptRoot "build-deerflow-$entry.ps1") -Destination $wrapper
        $target = if ($entry -eq 'desktop') { 'build-deerflow-desktop-go.ps1' } else { 'build-go-acp-portable.ps1' }
        @'
param([string]$Configuration,[string]$OutputDirectory,[string]$DesktopTargetDirectory,[switch]$SkipBuild,[switch]$CreateZip)
[pscustomobject]@{ Configuration=$Configuration; OutputDirectory=$OutputDirectory; DesktopTargetDirectory=$DesktopTargetDirectory; SkipBuild=[bool]$SkipBuild; CreateZip=[bool]$CreateZip }
'@ | Set-Content -LiteralPath (Join-Path $fixture $target) -Encoding utf8
        $default = & $wrapper -WarningAction SilentlyContinue
        Assert-Fixture ($default.Configuration -eq 'Release' -and $default.CreateZip) "$entry default delegation failed"
        $custom = & $wrapper -Configuration Debug -OutputDirectory 'fixture-output' -SkipZip -WarningAction SilentlyContinue
        Assert-Fixture ($custom.Configuration -eq 'Debug' -and $custom.OutputDirectory -eq 'fixture-output' -and -not $custom.CreateZip) "$entry explicit delegation failed"
        if ($entry -eq 'desktop') {
            $skip = & $wrapper -SkipBuild -SkipZip -DesktopTargetDirectory 'fixture-target' -WarningAction SilentlyContinue
            Assert-Fixture ($skip.SkipBuild -and -not $skip.CreateZip -and $skip.DesktopTargetDirectory -eq 'fixture-target') 'desktop skip delegation failed'
        }
    }
    . (Join-Path $PSScriptRoot 'go-package-inputs.ps1')
    Assert-GoRepositorySources -RepositoryRoot $repository
    $skills = Join-Path $fixture 'skills'
    New-Item -ItemType Directory -Path $skills | Out-Null
    Set-Content -LiteralPath (Join-Path $skills 'SKILL.md') -Value '# Native skill' -Encoding utf8
    Assert-GoBundledSkills -Source $skills
    foreach ($content in @('python scripts/run.py', '`python3 scripts/run.py`', 'pip install package', './scripts/run.py')) {
        Set-Content -LiteralPath (Join-Path $skills 'SKILL.md') -Value $content -Encoding utf8
        Assert-GoBundledSkills -Source $skills
    }
    Set-Content -LiteralPath (Join-Path $skills 'SKILL.md') -Value 'Document Python projects with `grep --include="*.py"`.' -Encoding utf8
    Assert-GoBundledSkills -Source $skills
    Set-Content -LiteralPath (Join-Path $skills 'SKILL.md') -Value "``````python`n# Documentation language example`n``````" -Encoding utf8
    Assert-GoBundledSkills -Source $skills
    Set-Content -LiteralPath (Join-Path $skills 'run.py') -Value 'fixture' -Encoding utf8
    Set-Content -LiteralPath (Join-Path $skills 'requirements.txt') -Value 'paramiko>=3' -Encoding utf8
    Assert-GoBundledSkills -Source $skills
    foreach ($name in @('run.pyc', 'run.pyd', 'package.whl', 'python.exe', 'pyvenv.cfg')) {
        $file = Join-Path $skills $name
        Set-Content -LiteralPath $file -Value 'fixture' -Encoding utf8
        $rejected = $false
        try { Assert-GoBundledSkills -Source $skills } catch { $rejected = $true }
        Assert-Fixture $rejected "Compiled runtime accepted: $name"
        Remove-Item -LiteralPath $file
    }
    New-Item -ItemType Directory -Path (Join-Path $skills '.venv') | Out-Null
    $rejected = $false
    try { Assert-GoBundledSkills -Source $skills } catch { $rejected = $true }
    Assert-Fixture $rejected 'Runtime environment accepted'
    Write-Host 'Go build entry points: syntax, delegation, Python skill source acceptance and runtime exclusion passed.'
} finally {
    $resolvedFixture = [IO.Path]::GetFullPath($fixture)
    if ($resolvedFixture.StartsWith([IO.Path]::GetFullPath($cache) + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
        Remove-Item -LiteralPath $resolvedFixture -Recurse -Force
    }
}
