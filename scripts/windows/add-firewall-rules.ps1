#Requires -Version 5.1
#Requires -RunAsAdministrator
<#
.SYNOPSIS
  Narrow Private-network UDP rules for remote-au receiver (program-scoped inbound).
.NOTES
  Sender ephemeral reply traffic is covered by stateful outbound; outbound rules use RemotePort only.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string] $ProgramPath,
    [int[]] $AudioPorts = @(47000),
    [int[]] $DiscoveryPorts = @(47001, 48001, 49001)
)

$ErrorActionPreference = 'Stop'
$Profile = 'Private'
$Prefix = 'remote-au'

if (-not (Test-Path -LiteralPath $ProgramPath)) {
    throw "ProgramPath not found: $ProgramPath"
}
$ProgramPath = (Resolve-Path -LiteralPath $ProgramPath).Path

function Add-ProgramUdpInboundRule([string] $Name, [int] $LocalPort) {
    $existing = Get-NetFirewallRule -DisplayName $Name -ErrorAction SilentlyContinue
    if ($existing) {
        Write-Host "Rule exists: $Name"
        return
    }
    New-NetFirewallRule -DisplayName $Name -Direction Inbound -Action Allow -Protocol UDP `
        -Program $ProgramPath -LocalPort $LocalPort -RemoteAddress LocalSubnet `
        -Profile $Profile -Description 'remote-au receiver LAN audio/discovery (program-scoped)' | Out-Null
    Write-Host "Added $Name (inbound UDP local $LocalPort, $Profile, LocalSubnet)"
}

function Add-ProgramUdpOutboundRemotePortRule([string] $Name, [int] $RemotePort) {
    $existing = Get-NetFirewallRule -DisplayName $Name -ErrorAction SilentlyContinue
    if ($existing) {
        Write-Host "Rule exists: $Name"
        return
    }
    New-NetFirewallRule -DisplayName $Name -Direction Outbound -Action Allow -Protocol UDP `
        -Program $ProgramPath -RemotePort $RemotePort -Profile $Profile `
        -Description 'remote-au discovery/outbound to receiver ports (RemotePort)' | Out-Null
    Write-Host "Added $Name (outbound UDP remote $RemotePort, $Profile)"
}

foreach ($p in $AudioPorts) {
    Add-ProgramUdpInboundRule "$Prefix-udp-in-$p" $p
    Add-ProgramUdpOutboundRemotePortRule "$Prefix-udp-out-remote-$p" $p
}
foreach ($p in $DiscoveryPorts) {
    Add-ProgramUdpInboundRule "$Prefix-disc-in-$p" $p
    Add-ProgramUdpOutboundRemotePortRule "$Prefix-disc-out-remote-$p" $p
}

Write-Host "Inbound rules are program-scoped for receiver; sender reply UDP uses stateful matching."
Write-Host "The Rust daemon uses UDP only. Public/Domain profiles are unchanged."
