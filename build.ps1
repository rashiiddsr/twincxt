# Builds build\Twincxt.exe and the installer dist\Twincxt-Setup-<version>.exe
#
# Requirements: Go, Python 3, Inno Setup 6.
#   winget install GoLang.Go Python.Python.3.12 JRSoftware.InnoSetup
#
# Usage:  .\build.ps1                 (version 1.0.0)
#         .\build.ps1 -Version 1.2.0
#         .\build.ps1 -SkipInstaller   (exe only)

param(
    [string]$Version = "1.0.0",
    [switch]$SkipInstaller
)

$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

function Step($msg) { Write-Host "==> $msg" -ForegroundColor Green }

Step "Generating icon"
python gen_icon.py
if ($LASTEXITCODE -ne 0) { throw "gen_icon.py failed" }

Step "Embedding icon, manifest and version info"
python gen_resources.py $Version
if ($LASTEXITCODE -ne 0) { throw "gen_resources.py failed" }

Step "Building build\Twincxt.exe"
New-Item -ItemType Directory -Force build, dist | Out-Null
go build -mod=vendor -trimpath -ldflags "-X main.appVersion=$Version -H windowsgui -s -w" -o build\Twincxt.exe .
if ($LASTEXITCODE -ne 0) { throw "go build failed" }

if ($SkipInstaller) { Step "Done (installer skipped)"; exit 0 }

Step "Building installer"
$iscc = @(
    (Get-Command ISCC.exe -ErrorAction SilentlyContinue).Source,
    "$env:LOCALAPPDATA\Programs\Inno Setup 6\ISCC.exe",
    "${env:ProgramFiles(x86)}\Inno Setup 6\ISCC.exe",
    "$env:ProgramFiles\Inno Setup 6\ISCC.exe"
) | Where-Object { $_ -and (Test-Path $_) } | Select-Object -First 1
if (-not $iscc) { throw "Inno Setup 6 (ISCC.exe) not found. Install it: winget install JRSoftware.InnoSetup" }

& $iscc /Qp "/DAppVersion=$Version" installer.iss
if ($LASTEXITCODE -ne 0) { throw "ISCC failed" }

Step "Done: dist\Twincxt-Setup-$Version.exe"
