#Requires -Version 5.1
<#
.SYNOPSIS
  Manual Windows role/task helpers (no install side effects).
.EXAMPLE
  .\daemon-task-commands.ps1 -Action status
  .\daemon-task-commands.ps1 -Action start
  .\daemon-task-commands.ps1 -Action stop
#>
[CmdletBinding()]
param(
    [ValidateSet('status', 'start', 'stop', 'receiver-hint')]
    [string] $Action = 'status'
)

$ErrorActionPreference = 'Stop'
$AppName = 'remote-au'
$TaskName = "$AppName-daemon"
$Root = Join-Path $env:LOCALAPPDATA $AppName
$Exe = Join-Path $Root 'bin\remote-au.exe'
$ConfigDir = Join-Path $Root 'config'

switch ($Action) {
    'status' {
        $task = Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
        if (-not $task) {
            Write-Host "Sender task '$TaskName' not registered."
            exit 0
        }
        $info = Get-ScheduledTaskInfo -TaskName $TaskName
        Write-Host "Task: $TaskName  State=$($task.State)  LastResult=$($info.LastTaskResult)"
    }
    'start' {
        Start-ScheduledTask -TaskName $TaskName
        Write-Host "Started $TaskName"
    }
    'stop' {
        Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue | Out-Null
        Write-Host "Stopped $TaskName (process may take a moment to exit)"
    }
    'receiver-hint' {
        Write-Host "Receiver mode (manual, same binary):"
        Write-Host "  1. Stop sender: .\daemon-task-commands.ps1 -Action stop"
        Write-Host "  2. Admin firewall: .\add-firewall-rules.ps1 -ProgramPath '$Exe'"
        Write-Host "  3. Pair on Mac/server first; then run:"
        Write-Host "     & '$Exe' --config-dir '$ConfigDir' server"
        Write-Host "  4. To return to sender: unregister server, Start-ScheduledTask -TaskName '$TaskName'"
    }
}
