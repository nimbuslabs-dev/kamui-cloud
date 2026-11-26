[CmdletBinding()]
param()
. (Join-Path $PSScriptRoot 'ops/kamui.ps1')
try {
    $config = Get-KamuiConfig
    Invoke-KamuiCompose stop
    $ids = @(Invoke-KamuiDocker ps -q --filter "label=dev.nimbuslabs.kamui.project=$($config.Project)" `
        --filter 'label=dev.nimbuslabs.kamui.workspace')
    if ($ids.Count -gt 0) { Invoke-KamuiDocker stop @ids }
    Write-Host "Stopped $($config.Project) services and workspaces. Volumes are preserved."
    exit 0
} catch {
    [Console]::Error.WriteLine($_.Exception.Message)
    exit 1
}
