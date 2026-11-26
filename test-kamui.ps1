[CmdletBinding()]
param()
. (Join-Path $PSScriptRoot 'ops/kamui.ps1')
try {
    Assert-KamuiDocker
    $config = Get-KamuiConfig
    Invoke-KamuiCompose config --quiet
    Invoke-KamuiCompose exec -T workspace-proxy nginx -t
    Assert-KamuiReady $config
    Invoke-KamuiProbe $config '/workspace/ws-00000000/' '401'
    Invoke-KamuiProbe $config '/workspace/ws-00000000/' '401' @(
        'Connection: Upgrade', 'Upgrade: websocket', 'Sec-WebSocket-Version: 13',
        'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==', "Origin: $($config.Url)"
    )
    Write-Host 'PASS. Service health, trusted HTTPS and anonymous API, IDE and WebSocket rejection.'
    exit 0
} catch {
    [Console]::Error.WriteLine($_.Exception.Message)
    exit 1
}
