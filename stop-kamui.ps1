<#
.SYNOPSIS
    Stop Kamui Cloud services without deleting data.

.DESCRIPTION
    Stops all Kamui Cloud services gracefully while preserving persistent volumes.
    Workspaces, API data, and Tailscale state are retained.

.NOTES
    Compatible with Windows PowerShell 5.1+
    Run from repository root: .\stop-kamui.ps1
#>

[CmdletBinding()]
param(
    [string]$ComposeFile = "ops/docker-compose.yml",
    [switch]$RemoveOrphans
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

if (-not (Test-Path $ComposePath)) {
    Write-Error "docker-compose.yml not found at $ComposePath"
    exit 1
}

Write-Host "╔══════════════════════════════════════════════════════════════╗" -ForegroundColor $Cyan
Write-Host "║         KAMUI CLOUD - STOPPING SERVICES                     ║" -ForegroundColor $Cyan
Write-Host "╚══════════════════════════════════════════════════════════════╝" -ForegroundColor $Cyan
Write-Host ""

Write-Info "Stopping Kamui Cloud services..."

$downArgs = @("compose", "-f", $ComposePath, "down")
if ($RemoveOrphans) { $downArgs += "--remove-orphans" }

try {
    $result = docker @downArgs 2>&1
    if ($LASTEXITCODE -eq 0) {
        Write-Success "All services stopped gracefully"
    } else {
        Write-Error "Stop failed with exit code $LASTEXITCODE"
        Write-Host $result -ForegroundColor $Red
        exit 1
    }
} catch {
    Write-Error "Stop failed: $_"
    exit 1
}

Write-Host ""
Write-Success "Kamui Cloud stopped. Persistent data preserved:"
Write-Host "  • kamui-api-data (API database)" -ForegroundColor $Cyan
Write-Host "  • kamui-workspaces (workspace source code & git history)" -ForegroundColor $Cyan
Write-Host "  • tailscale-state (Tailscale identity)" -ForegroundColor $Cyan
Write-Host ""
Write-Info "To restart: .\start-kamui.ps1"
Write-Info "To remove ALL data (including volumes): docker compose -f $ComposePath down -v"

exit 0