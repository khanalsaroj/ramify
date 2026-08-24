# Ramify installer for Windows (PowerShell) — installs both ramify.exe and ramifyd.exe.
#
#   iwr -useb https://raw.githubusercontent.com/khanalsaroj/ramify/main/scripts/install.ps1 | iex
#
# Environment overrides:
#   $env:RAMIFY_VERSION       install a specific version (e.g. v0.3.1), default: latest
#   $env:RAMIFY_INSTALL_DIR   install location, default: %USERPROFILE%\.ramify\bin
$ErrorActionPreference = "Stop"

$Repo = "khanalsaroj/ramify"
$Bins = @("ramify", "ramifyd")
$InstallDir = if ($env:RAMIFY_INSTALL_DIR) { $env:RAMIFY_INSTALL_DIR } else { "$env:USERPROFILE\.ramify\bin" }

function Info($m) { Write-Host "  $m" }
function Ok($m)   { Write-Host "  $m" -ForegroundColor Green }
function Warn($m) { Write-Host "  $m" -ForegroundColor Yellow }
function Die($m)  { Write-Host "  $m" -ForegroundColor Red; exit 1 }

Write-Host ""
Write-Host "  ramify" -ForegroundColor Green -NoNewline
Write-Host " - every branch becomes a live URL"
Write-Host ""

# ---------- Architecture ----------
# Windows release archives are amd64 only for now (see 'From source' in the README for other archs).
$Arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    "AMD64" { "amd64" }
    default { Die "unsupported architecture: $env:PROCESSOR_ARCHITECTURE (ramify ships windows/amd64 only - see 'From source' in the README)" }
}

# ---------- Version ----------
if ($env:RAMIFY_VERSION) {
    $Version = $env:RAMIFY_VERSION.TrimStart("v")
} else {
    try {
        $Version = (Invoke-RestMethod "https://api.github.com/repos/$Repo/releases/latest").tag_name.TrimStart("v")
    } catch {
        Die "Could not resolve the latest release - has one been published yet? See https://github.com/$Repo/releases ($_)"
    }
}
Info "Installing ramify + ramifyd v$Version for windows/$Arch"

$Asset = "ramify-windows-$Arch.zip"
$Url = "https://github.com/$Repo/releases/download/v$Version/$Asset"

$Tmp = Join-Path $env:TEMP ("ramify-" + [System.Guid]::NewGuid().ToString())
New-Item -ItemType Directory -Force -Path $Tmp | Out-Null
$Zip = Join-Path $Tmp $Asset

Info "Downloading $Url"
try {
    Invoke-WebRequest -Uri $Url -OutFile $Zip -UseBasicParsing
} catch {
    Die "Download failed - does a release exist for windows/$Arch? ($_)"
}

# ---------- Checksum verification (best effort) ----------
try {
    $SumsUrl = "https://github.com/$Repo/releases/download/v$Version/checksums.txt"
    $Sums = (Invoke-WebRequest -Uri $SumsUrl -UseBasicParsing).Content
    $Line = ($Sums -split "`n") | Where-Object { $_ -match [regex]::Escape($Asset) } | Select-Object -First 1
    if ($Line) {
        $Expected = ($Line.Trim() -split "\s+")[0]
        $Actual = (Get-FileHash -Algorithm SHA256 -Path $Zip).Hash
        if ($Expected -and ($Expected.ToLower() -ne $Actual.ToLower())) {
            Die "Checksum mismatch for $Asset"
        }
        Ok "Checksum verified"
    } else {
        Warn "No checksum entry for $Asset - skipping verification"
    }
} catch {
    Warn "Skipping checksum verification ($_)"
}

# ---------- Extract ----------
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
Expand-Archive -Force -Path $Zip -DestinationPath $InstallDir

# ---------- PATH ----------
$UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (-not $UserPath) { $UserPath = "" }
if ($UserPath -notlike "*$InstallDir*") {
    [Environment]::SetEnvironmentVariable("Path", ($UserPath.TrimEnd(";") + ";" + $InstallDir), "User")
    Warn "Added $InstallDir to your PATH. Restart your terminal to pick it up."
}
$env:Path = "$env:Path;$InstallDir"

Remove-Item -Recurse -Force $Tmp -ErrorAction SilentlyContinue

# ---------- Verify ----------
$AllFound = $true
foreach ($bin in $Bins) {
    $Exe = Join-Path $InstallDir "$bin.exe"
    if (Test-Path $Exe) {
        Ok "Installed: $Exe"
    } else {
        Warn "Missing: $Exe"
        $AllFound = $false
    }
}

if ($AllFound) {
    & (Join-Path $InstallDir "ramify.exe") --version
    Write-Host ""
    Ok "Done! Next: ramify install --config-dir `$env:ProgramData\ramify --data-dir `$env:ProgramData\ramify\data"
    Ok "See: https://github.com/$Repo/blob/main/docs/quickstart.md"
} else {
    Die "Installation incomplete - one or more binaries were not found in the archive"
}
