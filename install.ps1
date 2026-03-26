# NME Print Bridge — Windows Installer
# Usage: irm https://raw.githubusercontent.com/narbada-madhusudhan/nme-print-bridge/main/install.ps1 | iex

$ErrorActionPreference = "Stop"
$Repo = "narbada-madhusudhan/nme-print-bridge"
$InstallDir = "$env:LOCALAPPDATA\NME Print Bridge"
$ExePath = "$InstallDir\nme-print-bridge.exe"
$Binary = "print-bridge-windows-amd64.exe"
$URL = "https://github.com/$Repo/releases/latest/download/$Binary"

# Helper: kill all running instances of the bridge
function Stop-Bridge {
    # Try multiple ways — GUI-mode processes don't always show by name
    Stop-Process -Name "nme-print-bridge" -Force -ErrorAction SilentlyContinue
    taskkill /F /IM "nme-print-bridge.exe" 2>$null | Out-Null
    # Also kill by path in case the process name is mangled
    Get-Process | Where-Object { $_.Path -like "*nme-print-bridge*" } | Stop-Process -Force -ErrorAction SilentlyContinue
    Start-Sleep -Seconds 1
}

# Helper: remove the exe, with rename fallback if locked
function Remove-Bridge {
    Remove-Item $ExePath -Force -ErrorAction SilentlyContinue
    if (Test-Path $ExePath) {
        # File still locked — rename it so we can download fresh; Windows will clean up on reboot
        $OldPath = "$ExePath.old"
        Remove-Item $OldPath -Force -ErrorAction SilentlyContinue
        Rename-Item $ExePath $OldPath -Force -ErrorAction SilentlyContinue
    }
}

# Handle --uninstall
if ($args -contains "--uninstall" -or $args -contains "uninstall") {
    Write-Host "`n  Uninstalling NME Print Bridge..."
    Stop-Bridge
    if (Test-Path $ExePath) {
        & $ExePath --uninstall 2>$null
        Stop-Bridge
        Remove-Bridge
        Write-Host "  OK Uninstalled" -ForegroundColor Green
    } else {
        Write-Host "  Not installed at $ExePath"
    }
    exit 0
}

Write-Host ""
Write-Host "  =======================================" -ForegroundColor Cyan
Write-Host "     NME Print Bridge - Installer        " -ForegroundColor Cyan
Write-Host "  =======================================" -ForegroundColor Cyan
Write-Host ""

# Stop existing if upgrading
if (Test-Path $ExePath) {
    Write-Host "  -> Stopping existing installation..."
    Stop-Bridge
    & $ExePath --uninstall 2>$null
    Stop-Bridge
    Remove-Bridge
    if ((Test-Path $ExePath) -and -not (Test-Path "$ExePath.old")) {
        Write-Host "  X Could not remove old binary. Close any running instances and retry." -ForegroundColor Red
        exit 1
    }
}

# Clean up old renamed binary
Remove-Item "$ExePath.old" -Force -ErrorAction SilentlyContinue

# Download
Write-Host "  -> Downloading latest release..."
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
$ExePath = "$InstallDir\nme-print-bridge.exe"
Invoke-WebRequest -Uri $URL -OutFile $ExePath -UseBasicParsing
Unblock-File -Path $ExePath

if (-not (Test-Path $ExePath)) {
    Write-Host "  X Download failed" -ForegroundColor Red
    exit 1
}

Write-Host "  OK Downloaded to $ExePath" -ForegroundColor Green

# Install auto-start and start the service
Write-Host "  -> Setting up auto-start..."
& $ExePath --install
Write-Host "  -> Starting bridge..."
Start-Process -FilePath $ExePath -WindowStyle Hidden

# Verify the bridge is actually running
Write-Host "  -> Verifying bridge is running..."
$Running = $false
for ($i = 0; $i -lt 5; $i++) {
    Start-Sleep -Seconds 1
    try {
        $Response = Invoke-WebRequest -Uri "http://localhost:9120/health" -UseBasicParsing -TimeoutSec 3
        if ($Response.StatusCode -eq 200) {
            $Running = $true
            break
        }
    } catch {
        # Not ready yet, retry
    }
}

Write-Host ""
if ($Running) {
    Write-Host "  =======================================" -ForegroundColor Green
    Write-Host "  OK Installation complete!              " -ForegroundColor Green
    Write-Host "                                         "
    Write-Host "  NME Print Bridge is now running and    "
    Write-Host "  will start automatically on login.     "
    Write-Host "                                         "
    Write-Host "  Status: http://localhost:9120          "
    Write-Host "                                         "
    Write-Host "  To uninstall:                          "
    Write-Host "  & '$ExePath' --uninstall               "
    Write-Host "  =======================================" -ForegroundColor Green
} else {
    Write-Host "  =======================================" -ForegroundColor Yellow
    Write-Host "  ! Installation finished but bridge     " -ForegroundColor Yellow
    Write-Host "    did not respond on port 9120.        " -ForegroundColor Yellow
    Write-Host "                                         "
    Write-Host "  Try running manually to see errors:    "
    Write-Host "  & '$ExePath'                           "
    Write-Host "                                         "
    Write-Host "  Common fixes:                          "
    Write-Host "  - Allow through Windows Firewall       "
    Write-Host "  - Allow in Windows Defender/antivirus  "
    Write-Host "  - Run PowerShell as Administrator      "
    Write-Host "  =======================================" -ForegroundColor Yellow
}
Write-Host ""
