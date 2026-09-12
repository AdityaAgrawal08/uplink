<#pragma warning disable
# Uplink installer for Windows (PowerShell 5.1+).
# Usage: irm https://raw.githubusercontent.com/AdityaAgrawal08/uplink-delta/main/install.ps1 | iex
$ErrorActionPreference = "Stop"

$REPO = "AdityaAgrawal08/uplink-delta"
$BINARY_NAME = "uplink"

# Detect architecture
$ARCH = switch ($env:PROCESSOR_ARCHITECTURE) {
    "AMD64" { "amd64" }
    "ARM64" { "arm64" }
    default { Write-Error "Unsupported architecture: $($env:PROCESSOR_ARCHITECTURE)"; exit 1 }
}

$ASSET_NAME = "$BINARY_NAME-windows-$ARCH.zip"
$DOWNLOAD_URL = "https://github.com/$REPO/releases/latest/download/$ASSET_NAME"
$CHECKSUM_URL = "https://github.com/$REPO/releases/latest/download/checksums.txt"

$TEMP_DIR = Join-Path $env:TEMP ("uplink-install-" + [Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $TEMP_DIR | Out-Null
try {
    Write-Host "Downloading $DOWNLOAD_URL ..."
    Invoke-WebRequest -Uri $DOWNLOAD_URL -OutFile (Join-Path $TEMP_DIR $ASSET_NAME)

    Write-Host "Verifying checksum ..."
    $expected = (Invoke-WebRequest -Uri $CHECKSUM_URL -UseBasicParsing).Content -split "`n" |
        Where-Object { $_ -match [regex]::Escape($ASSET_NAME) } |
        ForEach-Object { ($_ -split '\s+')[0] }
    if (-not $expected) { throw "Checksum entry for $ASSET_NAME not found in checksums.txt" }
    $actual = (Get-FileHash -Path (Join-Path $TEMP_DIR $ASSET_NAME) -Algorithm SHA256).Hash.ToLower()
    if ($actual -ne $expected.Trim().ToLower()) { throw "Checksum mismatch — aborting (do not run this binary)" }
    Write-Host "Checksum OK."

    Write-Host "Extracting ..."
    Expand-Archive -Path (Join-Path $TEMP_DIR $ASSET_NAME) -DestinationPath $TEMP_DIR -Force

    $INSTALL_DIR = Join-Path $env:LOCALAPPDATA "uplink"
    New-Item -ItemType Directory -Path $INSTALL_DIR -Force | Out-Null
    Copy-Item -Path (Join-Path $TEMP_DIR "$BINARY_NAME.exe") -Destination (Join-Path $INSTALL_DIR "$BINARY_NAME.exe") -Force

    # Ensure install dir is on the user PATH (no admin needed).
    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if ($userPath -notlike "*$INSTALL_DIR*") {
        [Environment]::SetEnvironmentVariable("Path", "$userPath;$INSTALL_DIR", "User")
        Write-Host "Added $INSTALL_DIR to your user PATH (restart the terminal to use it)."
    }
    Write-Host "Successfully installed to $INSTALL_DIR\$BINARY_NAME.exe."
    Write-Host "Run 'uplink' to start."
}
finally {
    Remove-Item -Recurse -Force $TEMP_DIR -ErrorAction SilentlyContinue
}
