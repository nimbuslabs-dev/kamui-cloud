[CmdletBinding()]
param()
. (Join-Path $PSScriptRoot 'ops/kamui.ps1')
try {
    Assert-KamuiDocker
    $config = Get-KamuiConfig
    Invoke-KamuiCompose ps -a
    Invoke-KamuiDocker ps -a --filter "label=dev.nimbuslabs.kamui.project=$($config.Project)"
    Assert-KamuiReady $config
    Write-Host "Verified HTTPS endpoint $($config.Url)"
    exit 0
} catch {
    [Console]::Error.WriteLine($_.Exception.Message)
    exit 1
}
