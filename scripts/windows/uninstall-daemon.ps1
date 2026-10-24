#Requires -Version 5.1
[CmdletBinding()]
param(
    [switch] $Purge
)

$ErrorActionPreference = 'Stop'

$AppName = 'remote-au'
$Root = Join-Path $env:LOCALAPPDATA $AppName
$BinDir = Join-Path $Root 'bin'
$Exe = Join-Path $BinDir 'remote-au.exe'
$TaskName = "$AppName-daemon"

function Stop-DaemonTaskAndWait([string] $Name, [string] $ExePath) {
    $task = Get-ScheduledTask -TaskName $Name -ErrorAction SilentlyContinue
    # Match only this installation's executable even if the task was already removed.
    $hadTask = [bool]$task
    try {
        Stop-ScheduledTask -TaskName $Name -ErrorAction SilentlyContinue | Out-Null
    } catch { }
    for ($i = 0; $i -lt 45; $i++) {
        $state = (Get-ScheduledTask -TaskName $Name -ErrorAction SilentlyContinue).State
        if ($state -ne 'Running') { break }
        Start-Sleep -Seconds 1
    }
    Get-Process -Name 'remote-au' -ErrorAction SilentlyContinue | ForEach-Object {
        if ($ExePath -and (Test-Path -LiteralPath $ExePath) -and $_.Path -eq $ExePath) {
            Stop-Process -Id $_.Id -Force -ErrorAction SilentlyContinue
        }
    }
    Start-Sleep -Seconds 1
    return $hadTask
}

$hadTask = Stop-DaemonTaskAndWait -Name $TaskName -ExePath $Exe
$task = Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
if ($task) {
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
    Write-Host "Removed scheduled task '$TaskName'."
} elseif ($hadTask) {
    Write-Host "Scheduled task '$TaskName' stopped (already unregistered)."
} else {
    Write-Host "Scheduled task '$TaskName' not found (already removed)."
}

if ($Purge) {
    if (Test-Path -LiteralPath $Root) {
        Remove-Item -LiteralPath $Root -Recurse -Force
        Write-Host "Purged $Root (config, logs, binary, wrapper)."
    }
} else {
    Write-Host "Kept $Root (config/pairing/logs/binary). Re-run with -Purge to delete."
}

Write-Host "Firewall: run uninstall-firewall-rules.ps1 as Administrator to remove owned rules only."
