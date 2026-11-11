# bps-server launcher for local development.
#
#   .\start-local.ps1              start the managed server (Ctrl+C to stop)
#   .\start-local.ps1 -Restart     stop any running instance first
#   .\start-local.ps1 -Status      show whether it is running
#   .\start-local.ps1 -Build       rebuild the binary before starting
#
# Listens on http://127.0.0.1:8790 (API) and http://127.0.0.1:8791/admin/ (console).
# The ModelTrace fingerprint bank is embedded in the binary, so attribution
# works without any extra file.

param(
    [switch]$Restart,
    [switch]$Status,
    [switch]$Build,
    [string]$Proxy = "http://127.0.0.1:7890",
    [string]$ApiAddr = "127.0.0.1:8790",
    [string]$AdminAddr = "127.0.0.1:8791",
    [string]$PublicBaseURL = ""
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$exe = Join-Path $root "bps-server.exe"
$dataDir = Join-Path $root "data"
$passwordFile = Join-Path $root "secrets\admin_password"

function Get-ServerProcess {
    Get-Process bps-server -ErrorAction SilentlyContinue
}

if ($Status) {
    $p = Get-ServerProcess
    if (-not $p) { "bps-server: not running"; exit 1 }
    foreach ($proc in $p) {
        "bps-server: pid=$($proc.Id) started=$($proc.StartTime)"
    }
    try {
        $h = (Invoke-WebRequest -UseBasicParsing "http://$ApiAddr/healthz" -TimeoutSec 5).Content
        "healthz: $h"
    } catch {
        "healthz: unreachable ($($_.Exception.Message))"
    }
    exit 0
}

if ($Restart) {
    Get-ServerProcess | ForEach-Object {
        "stopping pid=$($_.Id)"
        Stop-Process -Id $_.Id -Force
    }
    Start-Sleep -Seconds 2
}

if (-not (Test-Path $passwordFile)) {
    Write-Error "admin password file missing: $passwordFile"
    exit 1
}

if ($Build -or -not (Test-Path $exe)) {
    "building bps-server..."
    & go build -o $exe ./cmd/bps-server
    if ($LASTEXITCODE -ne 0) { Write-Error "build failed"; exit 1 }
}

# Keep the console on the same paths and data the earlier sessions used.
$env:BPS_DATA_DIR = $dataDir
$env:BPS_API_ADDR = $ApiAddr
$env:BPS_ADMIN_ADDR = $AdminAddr
$env:BPS_ADMIN_PASSWORD_FILE = $passwordFile
$env:BPS_LOCAL_PROXY = $Proxy
if ($PublicBaseURL) { $env:BPS_PUBLIC_BASE_URL = $PublicBaseURL }
# An empty value makes the loader use the embedded bank.
$env:BPS_MODELTRACE_BANK = ""

"bps-server starting"
"  api     http://$ApiAddr"
"  console http://$AdminAddr/admin/"
"  data    $dataDir"
"  proxy   $(if ($Proxy) { $Proxy } else { 'direct' })"
"  bank    embedded"
""
"press Ctrl+C to stop"

& $exe
