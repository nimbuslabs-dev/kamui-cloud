<#
.SYNOPSIS
    Report Kamui Cloud service health, URLs and diagnostics.

.DESCRIPTION
    Displays comprehensive status of all Kamui Cloud services including
    container health, resource usage, network connectivity, and access URLs.

.NOTES
    Compatible with Windows PowerShell 5.1+
    Run from repository root: .\status-kamui.ps1
#>

[CmdletBinding()]
param(
    [string]$ComposeFile = "ops/docker-compose.yml",
    [switch]$Watch
)

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Definition
$ComposePath = Join-Path $ScriptDir $ComposeFile

$Green = [ConsoleColor]::Green
$Yellow = [ConsoleColor]::Yellow
$Red = [ConsoleColor]::Red
$Cyan = [ConsoleColor]::Cyan
$Gray = [ConsoleColor]::Gray

function Write-Status {
    param([string]$Message, [ConsoleColor]$Color = $Cyan)
    $timestamp = Get-Date -Format "HH:mm:ss"
    Write-Host "[$timestamp] " -NoNewline -ForegroundColor $Gray
    Write-Host $Message -ForegroundColor $Color
}

function Write-Success { param([string]$Message) Write-Status "✓ $Message" $Green }
function Write-Warning { param([string]$Message) Write-Status "⚠ $Message" $Yellow }
function Write-Error { param([string]$Message) Write-Status "✗ $Message" $Red }
function Write-Info { param([string]$Message) Write-Status "ℹ $Message" $Cyan }

function Get-LanIP {
    try {
        # Get physical network adapters only (exclude virtual, loopback, tunnel adapters)
        $adapters = Get-NetAdapter -Physical 2>$null | Where-Object { $_.Status -eq "Up" }
        if ($adapters) {
            $ips = $adapters | Get-NetIPAddress -AddressFamily IPv4 | Where-Object { 
                $_.PrefixOrigin -in @("Dhcp", "Manual") -and 
                $_.IPAddress -notlike "127.*" -and 
                $_.IPAddress -notlike "169.254.*" -and
                $_.IPAddress -notlike "172.16.*" -and
                $_.IPAddress -notlike "172.17.*" -and
                $_.IPAddress -notlike "172.18.*" -and
                $_.IPAddress -notlike "172.19.*" -and
                $_.IPAddress -notlike "172.20.*" -and
                $_.IPAddress -notlike "172.21.*" -and
                $_.IPAddress -notlike "172.22.*" -and
                $_.IPAddress -notlike "172.23.*" -and
                $_.IPAddress -notlike "172.24.*" -and
                $_.IPAddress -notlike "172.25.*" -and
                $_.IPAddress -notlike "172.26.*" -and
                $_.IPAddress -notlike "172.27.*" -and
                $_.IPAddress -notlike "172.28.*" -and
                $_.IPAddress -notlike "172.29.*" -and
                $_.IPAddress -notlike "172.30.*" -and
                $_.IPAddress -notlike "172.31.*" -and
                $_.IPAddress -notlike "10.0.75.*" -and
                $_.IPAddress -notlike "10.255.*"
            } | Select-Object -ExpandProperty IPAddress
            if ($ips) { return $ips[0] }
        }
        
        # Fallback: try Get-NetIPAddress with more specific filtering
        $ips = Get-NetIPAddress -AddressFamily IPv4 | Where-Object { 
            $_.PrefixOrigin -in @("Dhcp", "Manual") -and 
            $_.IPAddress -notlike "127.*" -and 
            $_.IPAddress -notlike "169.254.*" -and
            $_.IPAddress -notlike "172.16.*" -and
            $_.IPAddress -notlike "172.17.*" -and
            $_.IPAddress -notlike "172.18.*" -and
            $_.IPAddress -notlike "172.19.*" -and
            $_.IPAddress -notlike "172.20.*" -and
            $_.IPAddress -notlike "172.21.*" -and
            $_.IPAddress -notlike "172.22.*" -and
            $_.IPAddress -notlike "172.23.*" -and
            $_.IPAddress -notlike "172.24.*" -and
            $_.IPAddress -notlike "172.25.*" -and
            $_.IPAddress -notlike "172.26.*" -and
            $_.IPAddress -notlike "172.27.*" -and
            $_.IPAddress -notlike "172.28.*" -and
            $_.IPAddress -notlike "172.29.*" -and
            $_.IPAddress -notlike "172.30.*" -and
            $_.IPAddress -notlike "172.31.*" -and
            $_.IPAddress -notlike "10.0.75.*" -and
            $_.IPAddress -notlike "10.255.*"
        } | Select-Object -ExpandProperty IPAddress
        if ($ips) { return $ips[0] }
        
        return "unknown"
    } catch {
        return "unknown"
    }
}

function Get-ServiceHealth {
    param([string]$ServiceName, [string]$ComposePath)
    try {
        $status = docker compose -f $ComposePath ps -a --format json $ServiceName 2>$null | ConvertFrom-Json
        if ($status) {
            return @{
                State = $status.State
                Health = if ($status.Health) { $status.Health } else { "none" }
            }
        }
    } catch {}
    return @{ State = "unknown"; Health = "unknown" }
}

function Show-Status {
    Clear-Host
    Write-Host "╔══════════════════════════════════════════════════════════════╗" -ForegroundColor $Cyan
    Write-Host "║         KAMUI CLOUD MOBILE WORKSPACE - STATUS               ║" -ForegroundColor $Cyan
    Write-Host "╚══════════════════════════════════════════════════════════════╝" -ForegroundColor $Cyan
    Write-Host ""
    
    if (-not (Test-Path $ComposePath)) {
        Write-Error "docker-compose.yml not found at $ComposePath"
        return
    }
    
    # Get LAN IP
    $lanIP = Get-LanIP
    
    # Show access URLs
    Write-Host "Access URLs:" -ForegroundColor $Green
    Write-Host "  Local:       http://localhost:8081" -ForegroundColor $Cyan
    Write-Host "  LAN (Phone): http://$lanIP:8081" -ForegroundColor $Cyan
    Write-Host "  API:         http://$lanIP:8080" -ForegroundColor $Cyan
    Write-Host "  Console:     http://$lanIP:3000" -ForegroundColor $Cyan
    Write-Host ""
    
    # Get container status
    Write-Host "Service Status:" -ForegroundColor $Green
    try {
        $containers = docker compose -f $ComposePath ps --format json 2>$null | ConvertFrom-Json
        if ($containers) {
            foreach ($c in $containers) {
                $name = $c.Name
                $state = $c.State
                $health = if ($c.Health) { $c.Health } else { "none" }
                $ports = if ($c.Ports) { $c.Ports } else { "N/A" }
                
                $color = switch ($state) {
                    "running" { $Green }
                    "exited" { $Red }
                    "created" { $Yellow }
                    "restarting" { $Yellow }
                    default { $Gray }
                }
                
                $healthColor = switch ($health) {
                    "healthy" { $Green }
                    "unhealthy" { $Red }
                    "starting" { $Yellow }
                    "none" { $Gray }
                    default { $Gray }
                }
                
                Write-Host "  $name" -NoNewline -ForegroundColor $color
                Write-Host "  [$state]" -NoNewline -ForegroundColor $color
                Write-Host "  Health: " -NoNewline -ForegroundColor $Gray
                Write-Host "$health" -ForegroundColor $healthColor
                Write-Host "  Ports: $ports" -ForegroundColor $Gray
            }
        }
    } catch {
        Write-Warning "Could not retrieve container status"
    }
    Write-Host ""
    
    # Resource usage
    Write-Host "Resource Usage:" -ForegroundColor $Green
    try {
        $stats = docker stats --no-stream --format "table {{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.MemPerc}}\t{{.NetIO}}\t{{.BlockIO}}" 2>$null
        Write-Host $stats
    } catch {
        Write-Warning "Could not retrieve resource stats"
    }
    Write-Host ""
    
    # Volumes
    Write-Host "Persistent Volumes:" -ForegroundColor $Green
    try {
        $volumes = docker volume ls --filter "name=kamui" --format "table {{.Name}}\t{{.Driver}}\t{{.Scope}}" 2>$null
        Write-Host $volumes
    } catch {
        Write-Warning "Could not retrieve volume info"
    }
    Write-Host ""
    
    # Network
    Write-Host "Networks:" -ForegroundColor $Green
    try {
        $networks = docker network ls --filter "name=kamui" --format "table {{.Name}}\t{{.Driver}}\t{{.Scope}}" 2>$null
        Write-Host $networks
    } catch {
        Write-Warning "Could not retrieve network info"
    }
    Write-Host ""
    
    # .env status
    $envPath = Join-Path $ScriptDir "ops/.env"
    if (Test-Path $envPath) {
        $envContent = Get-Content $envPath -Raw
        $hasToken = $envContent -match '(?m)^KAMUI_TOKEN\s*=\s*\S+'
        Write-Host "Configuration:" -ForegroundColor $Green
        Write-Host "  .env exists: Yes" -ForegroundColor $Green
        Write-Host "  KAMUI_TOKEN: $(if ($hasToken) { "Configured" } else { "Not set (using default)" })" -ForegroundColor $Cyan
    } else {
        Write-Host "Configuration:" -ForegroundColor $Yellow
        Write-Host "  .env: Not found (run start-kamui.ps1)" -ForegroundColor $Yellow
    }
}

# Main
if (-not (Test-Path $ComposePath)) {
    Write-Error "docker-compose.yml not found at $ComposePath"
    exit 1
}

if ($Watch) {
    Write-Info "Watch mode enabled (refresh every 5s). Press Ctrl+C to exit."
    while ($true) {
        Show-Status
        Start-Sleep -Seconds 5
    }
} else {
    Show-Status
}