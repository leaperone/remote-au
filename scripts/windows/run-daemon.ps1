#Requires -Version 5.1
# Generated/maintained by install-daemon.ps1; invoked via Scheduled Task:
#   powershell.exe -NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File "<absolute>\run-daemon.ps1"
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

$envFile = Join-Path $PSScriptRoot 'daemon-env.ps1'
if (-not (Test-Path -LiteralPath $envFile)) {
    Write-Error "Missing $envFile; re-run install-daemon.ps1"
    exit 1
}

. $envFile

if (-not $RemoteAuExe -or -not (Test-Path -LiteralPath $RemoteAuExe)) {
    Write-Error "remote-au.exe not found at '$RemoteAuExe'"
    exit 1
}

$logFile = $RemoteAuLogFile
$logDir = Split-Path -Parent $logFile
if (-not (Test-Path -LiteralPath $logDir)) {
    New-Item -ItemType Directory -Path $logDir -Force | Out-Null
}

$maxBytes = 5MB
if (Test-Path -LiteralPath $logFile) {
    $len = (Get-Item -LiteralPath $logFile).Length
    if ($len -gt $maxBytes) {
        $stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
        $rotated = "$logFile.$stamp"
        Move-Item -LiteralPath $logFile -Destination $rotated -Force
    }
}

if (-not $RemoteAuRole) { $RemoteAuRole = 'send' }
$argsList = @('--config-dir', $RemoteAuConfigDir, $RemoteAuRole, '--log-level', 'info')
if ($RemoteAuRole -eq 'send') {
    $argsList += @('--source', 'loopback')
    if ($RemoteAuReceiver) { $argsList += @('--to', $RemoteAuReceiver) }
}

# Rust diagnostics use stderr; Windows PowerShell must not treat them as terminating errors.
$ErrorActionPreference = 'Continue'
& $RemoteAuExe @argsList *>> $logFile
exit $LASTEXITCODE
