[CmdletBinding()]
param(
    [switch]$CleanBuild,
    [switch]$SkipBuild,
    [ValidateRange(10, 1800)][int]$HealthTimeoutSeconds = 180
)

. (Join-Path $PSScriptRoot 'ops/kamui.ps1')
try {
    if ($CleanBuild -and $SkipBuild) { throw 'Choose either CleanBuild or SkipBuild.' }
    Assert-KamuiDocker
    Initialize-KamuiEnvironment
    $config = Get-KamuiConfig
    Assert-KamuiPort $config
    if (-not $SkipBuild) {
        $build = @('build')
        if ($CleanBuild) { $build += @('--no-cache', '--pull') }
        Invoke-KamuiCompose @build workspace-image api workspace-manager console workspace-proxy
    }
    Invoke-KamuiCompose run --rm --no-deps tls-init
    Invoke-KamuiCompose up -d --no-build --wait --wait-timeout $HealthTimeoutSeconds
    Invoke-KamuiCompose exec -T workspace-proxy nginx -t
    # A renewed certificate is read even when Compose did not recreate nginx.
    Invoke-KamuiCompose exec -T workspace-proxy nginx -s reload
    Assert-KamuiReady $config
    Write-Host "Kamui is healthy at $($config.Url)"
    Write-Host "Trust the public CA on your own client before opening the browser: $($config.CA)"
    if ($config.BindIP -ne '127.0.0.1') {
        Write-Host 'Use a trusted private LAN. Reachability from other devices requires a separate client test.'
    }
    exit 0
} catch {
    [Console]::Error.WriteLine($_.Exception.Message)
    [Console]::Error.WriteLine('Run status-kamui.ps1 for service state and follow the Mobile Workspace runbook.')
    exit 1
}
