<#
.SYNOPSIS
    Run Kamui Cloud deployment smoke tests.

.DESCRIPTION
    Executes automated tests to verify the deployment is working correctly:
    - Docker environment validation
    - Service health checks
    - API endpoint testing
    - Workspace CRUD operations
    - Git operations
    - WebSocket connectivity

.NOTES
    Compatible with Windows PowerShell 5.1+
    Run from repository root: .\test-kamui.ps1
#>

[CmdletBinding()]
param(
    [string]$ComposeFile = "ops/docker-compose.yml",
    [string]$EnvFile = "ops/.env",
    [string]$BaseUrl = "http://localhost:8081",
    [int]$TimeoutSeconds = 30
)

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Definition
$ComposePath = Join-Path $ScriptDir $ComposeFile
$EnvPath = Join-Path $ScriptDir $EnvFile

$Green = [ConsoleColor]::Green
$Yellow = [ConsoleColor]::Yellow
$Red = [ConsoleColor]::Red
$Cyan = [ConsoleColor]::Cyan
$Gray = [ConsoleColor]::Gray

$TestResults = @()
$Passed = 0
$Failed = 0

function Write-Status {
    param([string]$Message, [ConsoleColor]$Color = $Cyan)
    $timestamp = Get-Date -Format "HH:mm:ss"
    Write-Host "[$timestamp] " -NoNewline -ForegroundColor $Gray
    Write-Host $Message -ForegroundColor $Color
}

function Write-Success { param([string]$Message) Write-Status "✓ PASS: $Message" $Green; $global:Passed++ }
function Write-Fail { param([string]$Message) Write-Status "✗ FAIL: $Message" $Red; $global:Failed++ }
function Write-Info { param([string]$Message) Write-Status "ℹ $Message" $Cyan }
function Write-Warning { param([string]$Message) Write-Status "⚠ $Message" $Yellow }

function Run-Test {
    param(
        [string]$Name,
        [scriptblock]$TestBlock
    )
    Write-Info "Running: $Name"
    try {
        $result = & $TestBlock
        if ($result) {
            Write-Success $Name
            $TestResults += @{Name=$Name; Result="PASS"}
        } else {
            Write-Fail $Name
            $TestResults += @{Name=$Name; Result="FAIL"}
        }
    } catch {
        Write-Fail "$Name - Exception: $_"
        $TestResults += @{Name=$Name; Result="ERROR"}
    }
}

function Invoke-Api {
    param(
        [string]$Method,
        [string]$Path,
        [string]$Token,
        [object]$Body = $null
    )
    $headers = @{}
    if ($Token) { $headers.Authorization = "Bearer $Token" }
    if ($Body) { 
        $headers["Content-Type"] = "application/json"
        $bodyJson = $Body | ConvertTo-Json -Depth 5
    } else {
        $bodyJson = $null
    }
    
    $uri = "$BaseUrl/api$Path"
    $params = @{
        Uri = $uri
        Method = $Method
        Headers = $headers
        TimeoutSec = $TimeoutSeconds
    }
    if ($bodyJson) { $params.Body = $bodyJson }
    
    try {
        $response = Invoke-RestMethod @params -ErrorAction Stop
        return @{ Success=$true; Data=$response; StatusCode=200 }
    } catch {
        $statusCode = if ($_.Exception.Response) { $_.Exception.Response.StatusCode.value__ } else { 0 }
        return @{ Success=$false; Error=$_.Exception.Message; StatusCode=$statusCode }
    }
}

# Check prerequisites
if (-not (Test-Path $ComposePath)) {
    Write-Error "docker-compose.yml not found"
    exit 1
}
if (-not (Test-Path $EnvPath)) {
    Write-Error ".env not found. Run start-kamui.ps1 first."
    exit 1
}

# Load token from .env
$envContent = Get-Content $EnvPath -Raw
$tokenMatch = [regex]::Match($envContent, '^KAMUI_TOKEN\s*=\s*(\S+)', [System.Text.RegularExpressions.RegexOptions]::Multiline)
if (-not $tokenMatch.Success) {
    Write-Error "KAMUI_TOKEN not found in .env"
    exit 1
}
$KAMUI_TOKEN = $tokenMatch.Groups[1].Value

Write-Host "╔══════════════════════════════════════════════════════════════╗" -ForegroundColor $Cyan
Write-Host "║         KAMUI CLOUD DEPLOYMENT SMOKE TESTS                  ║" -ForegroundColor $Cyan
Write-Host "╚══════════════════════════════════════════════════════════════╝" -ForegroundColor $Cyan
Write-Host ""

# Test 1: Docker Compose Services Running
Run-Test "Docker Compose services running" {
    $ps = docker compose -f $ComposePath ps --format json 2>$null | ConvertFrom-Json
    $running = $ps | Where-Object { $_.State -eq "running" } | Measure-Object
    $running.Count -ge 4  # api, console, workspace-proxy, workspace-manager
}

# Test 2: API Health Check
Run-Test "API /healthz endpoint" {
    $result = Invoke-Api "GET" "/healthz" $null
    $result.Success -and $result.Data.status -eq "ok"
}

# Test 3: API Authentication Required
Run-Test "API requires authentication" {
    $result = Invoke-Api "GET" "/v1/nodes" $null
    -not $result.Success -and $result.StatusCode -eq 401
}

# Test 4: Valid Token Authentication
Run-Test "Valid token authentication works" {
    $result = Invoke-Api "GET" "/v1/nodes" $KAMUI_TOKEN
    $result.Success
}

# Test 5: Workspace CRUD - Create
$testWorkspaceId = $null
Run-Test "Workspace Create" {
    $body = @{
        name = "smoke-test-workspace"
        repository_url = "https://github.com/octocat/Hello-World.git"
        branch = "main"
    }
    $result = Invoke-Api "POST" "/v1/workspaces" $KAMUI_TOKEN $body
    if ($result.Success -and $result.Data.id) {
        $script:testWorkspaceId = $result.Data.id
        $true
    } else {
        $false
    }
}

# Test 6: Workspace List
Run-Test "Workspace List" {
    $result = Invoke-Api "GET" "/v1/workspaces" $KAMUI_TOKEN
    $result.Success -and $result.Data -is [array]
}

# Test 7: Workspace Get
Run-Test "Workspace Get by ID" {
    if (-not $testWorkspaceId) { return $false }
    $result = Invoke-Api "GET" "/v1/workspaces/$testWorkspaceId" $KAMUI_TOKEN
    $result.Success -and $result.Data.id -eq $testWorkspaceId
}

# Test 8: Workspace Start
Run-Test "Workspace Start" {
    if (-not $testWorkspaceId) { return $false }
    $result = Invoke-Api "POST" "/v1/workspaces/$testWorkspaceId/start" $KAMUI_TOKEN
    $result.Success -and $result.Data.status -eq "running"
}

# Test 9: Wait for Workspace to be Ready
Run-Test "Workspace becomes ready" {
    if (-not $testWorkspaceId) { return $false }
    $start = Get-Date
    $timeout = New-TimeSpan -Seconds 60
    while ((Get-Date) - $start -lt $timeout) {
        $result = Invoke-Api "GET" "/v1/workspaces/$testWorkspaceId" $KAMUI_TOKEN
        if ($result.Success -and $result.Data.code_server_port -gt 0) {
            $script:testWorkspacePort = $result.Data.code_server_port
            return $true
        }
        Start-Sleep -Seconds 3
    }
    $false
}

# Test 10: Git Status in Running Workspace
Run-Test "Git Status endpoint" {
    if (-not $testWorkspaceId) { return $false }
    $result = Invoke-Api "GET" "/v1/workspaces/$testWorkspaceId/git/status" $KAMUI_TOKEN
    $result.Success -and $result.Data.branch
}

# Test 11: Workspace Stop
Run-Test "Workspace Stop" {
    if (-not $testWorkspaceId) { return $false }
    $result = Invoke-Api "POST" "/v1/workspaces/$testWorkspaceId/stop" $KAMUI_TOKEN
    $result.Success -and $result.Data.status -eq "stopped"
}

# Test 12: Workspace Delete (cleanup)
Run-Test "Workspace Delete (cleanup)" {
    if (-not $testWorkspaceId) { return $false }
    $result = Invoke-Api "DELETE" "/v1/workspaces/$testWorkspaceId" $KAMUI_TOKEN
    $result.Success -and $result.StatusCode -eq 204
}

# Test 13: WebSocket Upgrade Headers
Run-Test "Proxy WebSocket headers present" {
    try {
        $response = Invoke-WebRequest -Uri "$BaseUrl/healthz" -Method GET -TimeoutSec $TimeoutSeconds
        $hasUpgrade = $response.Headers["Upgrade"] -ne $null
        $hasConnection = $response.Headers["Connection"] -ne $null
        $hasUpgrade -or $hasConnection
    } catch {
        $false
    }
}

# Test 14: Console Accessible
Run-Test "Next.js Console accessible" {
    try {
        $response = Invoke-WebRequest -Uri "$BaseUrl/" -Method GET -TimeoutSec $TimeoutSeconds
        $response.StatusCode -eq 200 -and $response.Content -match "Kamui"
    } catch {
        $false
    }
}

# Test 15: API Proxy via Nginx
Run-Test "API accessible via proxy (/api/)" {
    $result = Invoke-Api "GET" "/healthz" $null  # This goes through nginx
    $result.Success
}

# Summary
Write-Host ""
Write-Host "╔══════════════════════════════════════════════════════════════╗" -ForegroundColor $Cyan
Write-Host "║                      TEST SUMMARY                            ║" -ForegroundColor $Cyan
Write-Host "╚══════════════════════════════════════════════════════════════╝" -ForegroundColor $Cyan
Write-Host ""

foreach ($r in $TestResults) {
    $color = if ($r.Result -eq "PASS") { $Green } else { $Red }
    Write-Host "  $($r.Result.PadRight(6)) $($r.Name)" -ForegroundColor $color
}

Write-Host ""
Write-Host "Total: $($TestResults.Count) | Passed: $Passed | Failed: $Failed" -ForegroundColor (if ($Failed -eq 0) { $Green } else { $Red })

if ($Failed -gt 0) {
    Write-Host ""
    Write-Error "Some tests failed. Check logs with: docker compose -f $ComposePath logs"
    exit 1
} else {
    Write-Host ""
    Write-Success "All smoke tests passed!"
    exit 0
}