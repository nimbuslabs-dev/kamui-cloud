<#
.SYNOPSIS
    Start Kamui Cloud Mobile Workspace MVP with one command.

.DESCRIPTION
    This script performs a complete, idempotent startup of the Kamui Cloud stack:
    - Validates Docker environment
    - Creates .env from .env.example if missing
    - Generates secure KAMUI_TOKEN if not present
    - Builds and starts all services
    - Waits for health checks
    - Reports LAN-accessible URLs

.NOTES
    Compatible with Windows PowerShell 5.1+
    Run from repository root: .\start-kamui.ps1

.EXAMPLE
    .\start-kamui.ps1
    .\start-kamui.ps1 -CleanBuild
    .\start-kamui.ps1 -SkipBuild
#>

[CmdletBinding()]
param(
    [switch]$CleanBuild,
    [switch]$SkipBuild,
    [switch]$Detach,
    [string]$ComposeFile = "ops/docker-compose.yml",
    [string]$EnvFile = "ops/.env",
    [string]$EnvExampleFile = "ops/.env.example",
    [int]$HealthTimeoutSeconds = 180
)

# Configuration
$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Definition
$ComposePath = Join-Path $ScriptDir $ComposeFile
$EnvPath = Join-Path $ScriptDir $EnvFile
$EnvExamplePath = Join-Path $ScriptDir $EnvExampleFile
$ProjectRoot = $ScriptDir

# Colors for output
$Green = [ConsoleColor]::Green
$Yellow = [ConsoleColor]::Yellow
$Red = [ConsoleColor]::Red
$Cyan = [ConsoleColor]::Cyan
$Gray = [ConsoleColor]::Gray
$Reset = [ConsoleColor]::Gray

function Write-Status {
    param([string]$Message, [ConsoleColor]$Color = $Cyan)
    $timestamp = Get-Date -Format "HH:mm:ss"
    Write-Host "[$timestamp] " -NoNewline -ForegroundColor $Gray
    Write-Host $Message -ForegroundColor $Color
}

function Write-Success {
    param([string]$Message)
    Write-Status "✓ $Message" $Green
}

function Write-Warning {
    param([string]$Message)
    Write-Status "⚠ $Message" $Yellow
}

function Write-Error {
    param([string]$Message)
    Write-Status "✗ $Message" $Red
}

function Write-Info {
    param([string]$Message)
    Write-Status "ℹ $Message" $Cyan
}

function Test-Docker {
    Write-Info "Checking Docker environment..."
    
    # Check Docker CLI
    try {
        $dockerVersion = docker version --format '{{.Client.Version}}' 2>$null
        if (-not $dockerVersion) {
            Write-Error "Docker CLI not found. Please install Docker Desktop."
            return $false
        }
        Write-Success "Docker CLI found: $dockerVersion"
    } catch {
        Write-Error "Docker CLI not available: $_"
        return $false
    }
    
    # Check Docker Compose
    try {
        $composeVersion = docker compose version --short 2>$null
        if (-not $composeVersion) {
            Write-Error "Docker Compose not found. Please install Docker Desktop with Compose plugin."
            return $false
        }
        Write-Success "Docker Compose found: $composeVersion"
    } catch {
        Write-Error "Docker Compose not available: $_"
        return $false
    }
    
    # Check Docker daemon
    try {
        $daemonInfo = docker info --format '{{.ServerVersion}}' 2>$null
        if (-not $daemonInfo) {
            Write-Error "Docker daemon not responding. Is Docker Desktop running?"
            return $false
        }
        Write-Success "Docker daemon running: $daemonInfo"
    } catch {
        Write-Error "Docker daemon not accessible: $_"
        return $false
    }
    
    return $true
}

function Test-PortAvailable {
    param([int]$Port, [string]$Service)
    $listener = New-Object System.Net.Sockets.TcpListener([System.Net.IPAddress]::IPv6Any, $Port)
    try {
        $listener.Start()
        $listener.Stop()
        return $true
    } catch {
        Write-Warning "Port $Port ($Service) appears to be in use"
        return $false
    }
}

function Validate-Ports {
    Write-Info "Checking required ports..."
    $ports = @(
        @{Port=8080; Service="API"},
        @{Port=3000; Service="Console"},
        @{Port=8081; Service="Workspace Proxy"}
    )
    
    $allAvailable = $true
    foreach ($p in $ports) {
        if (-not (Test-PortAvailable $p.Port $p.Service)) {
            $allAvailable = $false
        } else {
            Write-Success "Port $($p.Port) ($($p.Service)) available"
        }
    }
    return $allAvailable
}

function Setup-Environment {
    Write-Info "Setting up environment configuration..."
    
    if (-not (Test-Path $EnvExamplePath)) {
        Write-Error "Missing $EnvExampleFile"
        return $false
    }
    
    if (-not (Test-Path $EnvPath)) {
        Write-Info "Creating .env from .env.example..."
        Copy-Item $EnvExamplePath $EnvPath -Force
        Write-Success "Created .env"
    } else {
        Write-Success ".env already exists, preserving existing configuration"
    }
    
    # Load existing .env
    $envContent = Get-Content $EnvPath -Raw
    
    # Check if KAMUI_TOKEN needs generation
    if ($envContent -match '^KAMUI_TOKEN\s*=\s*(dev-change-me-in-production|$)' -or $envContent -notmatch '^KAMUI_TOKEN\s*=') {
        Write-Info "Generating secure KAMUI_TOKEN..."
        $bytes = New-Object byte[] 32
        $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
        $rng.GetBytes($bytes)
        $token = [System.BitConverter]::ToString($bytes).Replace('-', '').ToLower()
        
        # Update or add KAMUI_TOKEN
        if ($envContent -match '^KAMUI_TOKEN\s*=') {
            $envContent = $envContent -replace '^KAMUI_TOKEN\s*=.*', "KAMUI_TOKEN=$token"
        } else {
            $envContent += "`nKAMUI_TOKEN=$token"
        }
        
        Set-Content $EnvPath -Value $envContent -NoNewline
        Write-Success "Generated secure KAMUI_TOKEN (not displayed for security)"
    } else {
        Write-Success "KAMUI_TOKEN already configured"
    }
    
    return $true
}

function Build-Images {
    if ($SkipBuild) {
        Write-Warning "Skipping build (--SkipBuild specified)"
        return $true
    }
    
    Write-Info "Building Docker images..."
    $buildArgs = @("compose", "-f", $ComposePath)
    if ($CleanBuild) { $buildArgs += "--no-cache" }
    $buildArgs += "build"
    
    try {
        $result = docker @buildArgs 2>&1
        $lastExitCode = $LASTEXITCODE
        if ($lastExitCode -eq 0) {
            Write-Success "All images built successfully"
            return $true
        } else {
            Write-Error "Build failed with exit code $lastExitCode"
            Write-Host $result -ForegroundColor $Red
            return $false
        }
    } catch {
        Write-Error "Build failed: $_"
        return $false
    }
}

function Start-Services {
    Write-Info "Starting Kamui Cloud services..."
    $upArgs = @("compose", "-f", $ComposePath, "up")
    if ($Detach) { $upArgs += "-d" }
    
    try {
        $result = docker @upArgs 2>&1
        $lastExitCode = $LASTEXITCODE
        if ($lastExitCode -eq 0 -or $Detach) {
            Write-Success "Services started"
            return $true
        } else {
            Write-Error "Startup failed with exit code $lastExitCode"
            Write-Host $result -ForegroundColor $Red
            return $false
        }
    } catch {
        Write-Error "Startup failed: $_"
        return $false
    }
}

function Wait-For-Health {
    Write-Info "Waiting for services to become healthy (timeout: ${HealthTimeoutSeconds}s)..."
    $startTime = Get-Date
    $timeout = New-TimeSpan -Seconds $HealthTimeoutSeconds
    $services = @("api", "console", "workspace-proxy", "workspace-manager")
    $healthy = @{}
    
    while ((Get-Date) - $startTime -lt $timeout) {
        $allHealthy = $true
        foreach ($svc in $services) {
            if ($healthy.ContainsKey($svc)) { continue }
            
            try {
                $status = docker compose -f $ComposePath ps -a --format json $svc 2>$null | ConvertFrom-Json
                if ($status -and $status.Health -eq "healthy") {
                    $healthy[$svc] = $true
                    Write-Success "$svc is healthy"
                } elseif ($status -and $status.State -eq "running") {
                    Write-Status "  $svc: running (waiting for health check...)" $Yellow
                    $allHealthy = $false
                } else {
                    Write-Status "  $svc: $($status.State)" $Yellow
                    $allHealthy = $false
                }
            } catch {
                $allHealthy = $false
            }
        }
        
        if ($healthy.Count -eq $services.Count) {
            Write-Success "All services healthy!"
            return $true
        }
        
        Start-Sleep -Seconds 5
    }
    
    Write-Error "Health check timeout after ${HealthTimeoutSeconds}s"
    docker compose -f $ComposePath ps
    return $false
}

function Get-LanIP {
    try {
        $ips = Get-NetIPAddress -AddressFamily IPv4 | Where-Object { 
            $_.PrefixOrigin -ne "WellKnown" -and 
            $_.IPAddress -notlike "127.*" -and 
            $_.IPAddress -notlike "169.254.*" 
        } | Select-Object -ExpandProperty IPAddress
        return $ips[0]
    } catch {
        return "unknown"
    }
}

function Report-Status {
    Write-Host ""
    Write-Host "╔══════════════════════════════════════════════════════════════╗" -ForegroundColor $Cyan
    Write-Host "║           KAMUI CLOUD MOBILE WORKSPACE - READY               ║" -ForegroundColor $Cyan
    Write-Host "╚══════════════════════════════════════════════════════════════╝" -ForegroundColor $Cyan
    Write-Host ""
    
    $lanIP = Get-LanIP
    Write-Host "Access URLs:" -ForegroundColor $Green
    Write-Host "  Local:      http://localhost:8081" -ForegroundColor $Cyan
    Write-Host "  LAN (Phone): http://$lanIP:8081" -ForegroundColor $Cyan
    Write-Host ""
    Write-Host "Services:" -ForegroundColor $Green
    docker compose -f $ComposePath ps --format "table {{.Name}}\t{{.Status}}\t{{.Ports}}"
    Write-Host ""
    Write-Host "Next Steps:" -ForegroundColor $Green
    Write-Host "  1. Open http://$lanIP:8081 on your Android phone (same Wi-Fi)" -ForegroundColor $Cyan
    Write-Host "  2. Create a workspace with a GitHub repository URL" -ForegroundColor $Cyan
    Write-Host "  3. Click 'Start' then 'Open IDE' to launch code-server" -ForegroundColor $Cyan
    Write-Host "  4. Edit code, use terminal, commit and push to GitHub" -ForegroundColor $Cyan
    Write-Host ""
    Write-Host "Management Commands:" -ForegroundColor $Green
    Write-Host "  .\status-kamui.ps1   - Check service health and URLs" -ForegroundColor $Gray
    Write-Host "  .\stop-kamui.ps1     - Stop services (preserves data)" -ForegroundColor $Gray
    Write-Host "  .\test-kamui.ps1     - Run deployment smoke tests" -ForegroundColor $Gray
    Write-Host ""
    Write-Host "Security Notes:" -ForegroundColor $Yellow
    Write-Host "  • KAMUI_TOKEN is in ops/.env (keep secure!)" -ForegroundColor $Gray
    Write-Host "  • Workspace proxy on port 8081 requires authentication" -ForegroundColor $Gray
    Write-Host "  • Tailscale profile available for remote access: docker compose --profile remote up -d" -ForegroundColor $Gray
}

# Main execution
Write-Host "╔══════════════════════════════════════════════════════════════╗" -ForegroundColor $Cyan
Write-Host "║      KAMUI CLOUD MOBILE WORKSPACE MVP - STARTUP             ║" -ForegroundColor $Cyan
Write-Host "╚══════════════════════════════════════════════════════════════╝" -ForegroundColor $Cyan
Write-Host ""

# Validate working directory
if (-not (Test-Path $ComposePath)) {
    Write-Error "docker-compose.yml not found at $ComposePath"
    Write-Error "Run this script from the repository root"
    exit 1
}

# Run checks
if (-not (Test-Docker)) { exit 1 }
if (-not (Validate-Ports)) { 
    Write-Warning "Some ports may be in use. Continuing anyway..." 
}
if (-not (Setup-Environment)) { exit 1 }
if (-not (Build-Images)) { exit 1 }
if (-not (Start-Services)) { exit 1 }
if (-not (Wait-For-Health)) { exit 1 }

# Success!
Report-Status
exit 0