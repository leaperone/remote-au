#Requires -Version 5.1
#Requires -RunAsAdministrator
<#
.SYNOPSIS
  Removes only remote-au firewall rules created by add-firewall-rules.ps1 (exact display names).
#>
[CmdletBinding()]
param(
    [int[]] $AudioPorts = @(47000),
    [int[]] $DiscoveryPorts = @(47001, 48001, 49001)
)

$ErrorActionPreference = 'Stop'
$Prefix = 'remote-au'

$names = @()
foreach ($p in $AudioPorts) {
    $names += "$Prefix-udp-in-$p"
    $names += "$Prefix-udp-out-remote-$p"
}
foreach ($p in $DiscoveryPorts) {
    $names += "$Prefix-disc-in-$p"
    $names += "$Prefix-disc-out-remote-$p"
}

foreach ($name in $names) {
    $rule = Get-NetFirewallRule -DisplayName $name -ErrorAction SilentlyContinue
    if ($rule) {
        Remove-NetFirewallRule -DisplayName $name
        Write-Host "Removed rule: $name"
    } else {
        Write-Host "Rule not found (skip): $name"
    }
}

Write-Host "Done. No wildcard or broad rule removal performed."
