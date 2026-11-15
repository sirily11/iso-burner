$ErrorActionPreference = 'Stop'

$projectRoot = Split-Path -Parent $PSScriptRoot
$installDir = Join-Path $env:LOCALAPPDATA 'Programs\iso-burner'
$executable = Join-Path $installDir 'iso-burner.exe'
New-Item -ItemType Directory -Force -Path $installDir | Out-Null

Push-Location $projectRoot
try {
    & go build -trimpath -ldflags '-s -w' -o $executable .
    if ($LASTEXITCODE -ne 0) { throw 'Go build failed.' }
} finally {
    Pop-Location
}

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
$entries = @($userPath -split ';' | Where-Object { $_ -ne '' })
if (-not ($entries | Where-Object { $_.TrimEnd('\') -ieq $installDir.TrimEnd('\') })) {
    [Environment]::SetEnvironmentVariable('Path', (($entries + $installDir) -join ';'), 'User')
}
if (-not ($env:Path -split ';' | Where-Object { $_.TrimEnd('\') -ieq $installDir.TrimEnd('\') })) {
    $env:Path += ";$installDir"
}

Write-Host "Installed $executable"
Write-Host 'Open a new terminal, then run iso-burner from any folder.'
