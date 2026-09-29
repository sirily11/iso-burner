#Requires -RunAsAdministrator
param(
    [string]$Executable = (Join-Path $PSScriptRoot '..\bin\iso-bunner.exe'),
    [string]$ResultPath
)

$ErrorActionPreference = 'Stop'
try {
    $nativeExecutable = (Resolve-Path -LiteralPath $Executable).Path

    # Limit unauthenticated AirPrint traffic to devices on directly connected LANs.
    # Include Public because Windows may assign that profile to a home LAN.
    $ruleNames = @('IsoBurner-NativeAirPrint-IPP','IsoBurner-NativeAirPrint-mDNS')
    foreach ($ruleName in $ruleNames) {
        Get-NetFirewallRule -Name $ruleName -ErrorAction SilentlyContinue | Remove-NetFirewallRule
    }
    New-NetFirewallRule -Name $ruleNames[0] -DisplayName 'ISO Burner native AirPrint (IPP)' `
        -Direction Inbound -Action Allow -Program $nativeExecutable -Protocol TCP -LocalPort 8631 `
        -Profile Any -RemoteAddress LocalSubnet | Out-Null
    New-NetFirewallRule -Name $ruleNames[1] -DisplayName 'ISO Burner native AirPrint (Bonjour)' `
        -Direction Inbound -Action Allow -Program $nativeExecutable -Protocol UDP -LocalPort 5353 `
        -Profile Any -RemoteAddress LocalSubnet | Out-Null
    Write-Output 'Native AirPrint firewall rules installed for local-network devices.'
    if ($ResultPath) { [IO.File]::WriteAllText($ResultPath,'Installed') }
} catch {
    if ($ResultPath) { [IO.File]::WriteAllText($ResultPath,$_.Exception.Message) }
    throw
}
