# Shared by the entry points. Compatible with Windows PowerShell 5.1.
$ErrorActionPreference = 'Stop'
$script:EnvPath = Join-Path $PSScriptRoot '.env'
$script:KamuiServices = @('api', 'console', 'workspace-manager', 'workspace-proxy')
$script:Compose = @('compose', '--env-file', $script:EnvPath, '-f', (Join-Path $PSScriptRoot 'docker-compose.yml'))

function Invoke-KamuiDocker {
    & docker @args
    if ($LASTEXITCODE -ne 0) { throw "Docker command failed (exit $LASTEXITCODE)." }
}

function Invoke-KamuiCompose {
    Invoke-KamuiDocker @script:Compose @args
}

function Assert-KamuiDocker {
    if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
        throw 'Install Docker Desktop with Linux containers and WSL2, then start it.'
    }
    if ($env:OS -eq 'Windows_NT') {
        if (-not (Get-Command wsl.exe -ErrorAction SilentlyContinue)) {
            throw 'WSL is missing. Enable WSL2 and configure Docker Desktop to use it.'
        }
        & wsl.exe --status | Out-Null
        if ($LASTEXITCODE -ne 0) { throw 'WSL could not report its status. Check WSL2 and Docker Desktop.' }
    }
    Invoke-KamuiDocker compose version --short | Out-Null
    $info = Invoke-KamuiDocker info --format '{{.OSType}} {{.MemTotal}} {{.NCPU}}'
    if ($info -notmatch '^linux (\d+) (\d+)$') {
        throw 'Use a running Linux Docker Engine. On Windows select Docker Desktop Linux containers.'
    }
    if ([long]$Matches[1] -lt 3GB -or [int]$Matches[2] -lt 2) {
        throw 'Allocate at least 3 GiB of RAM and two CPUs to Docker for one workspace.'
    }
}

function New-KamuiSecret {
    $bytes = New-Object byte[] 32
    $rng = [Security.Cryptography.RandomNumberGenerator]::Create()
    try { $rng.GetBytes($bytes) } finally { $rng.Dispose() }
    return [BitConverter]::ToString($bytes).Replace('-', '').ToLowerInvariant()
}

function Initialize-KamuiEnvironment {
    $encoding = New-Object System.Text.UTF8Encoding($false)
    if (-not [IO.File]::Exists($script:EnvPath)) {
        $content = "KAMUI_TOKEN=$(New-KamuiSecret)`nKAMUI_ADMIN_PASSWORD=$(New-KamuiSecret)`n"
        $stream = New-Object IO.FileStream($script:EnvPath, [IO.FileMode]::CreateNew)
        try {
            if ($env:OS -ne 'Windows_NT') {
                & chmod 600 $script:EnvPath
                if ($LASTEXITCODE -ne 0) { throw 'Could not restrict access to ops/.env.' }
            }
            $bytes = $encoding.GetBytes($content)
            $stream.Write($bytes, 0, $bytes.Length)
        } finally { $stream.Dispose() }
        Write-Host 'Created ops/.env. Read KAMUI_ADMIN_PASSWORD from that file in a private editor.'
        return
    }

    $originalContent = [IO.File]::ReadAllText($script:EnvPath)
    $content = $originalContent
    $changes = $false
    foreach ($name in @('KAMUI_TOKEN', 'KAMUI_ADMIN_PASSWORD')) {
        if ([Environment]::GetEnvironmentVariable($name)) { continue }
        $assignment = "(?m)^[ \t]*(?:export[ \t]+)?${name}[ \t]*=[^\r\n]*"
        $entries = [regex]::Matches($content, $assignment)
        if ($entries.Count -eq 0) {
            $content += "`n$name=$(New-KamuiSecret)`n"
            $changes = $true
        } elseif ($entries.Count -eq 1 -and $entries[0].Value -match '=[ \t]*(?:#.*)?$') {
            $at = $entries[0].Index
            $content = $content.Remove($at, $entries[0].Length).Insert($at, "$name=$(New-KamuiSecret)")
            $changes = $true
        }
    }
    if ($changes) {
        # Replace the file only after the full update has been written beside it.
        $pending = $script:EnvPath + '.' + [Guid]::NewGuid().ToString('N')
        try {
            $original = [IO.File]::Open($script:EnvPath, 'Open', 'Read', 'Read')
            try {
                $reader = New-Object IO.StreamReader($original)
                try { $snapshot = $reader.ReadToEnd() } finally { $reader.Dispose() }
            } finally { $original.Dispose() }
            if ($snapshot -cne $originalContent) { throw 'ops/.env changed during startup. Retry the command.' }
            [IO.File]::WriteAllText($pending, "", $encoding)
            if ($env:OS -ne 'Windows_NT') {
                & chmod 600 $pending
                if ($LASTEXITCODE -ne 0) { throw 'Could not restrict access to the environment update.' }
            }
            [IO.File]::WriteAllText($pending, $content, $encoding)
            [IO.File]::Replace($pending, $script:EnvPath, [System.Management.Automation.Language.NullString]::Value)
        } finally {
            if ([IO.File]::Exists($pending)) { [IO.File]::Delete($pending) }
        }
        Write-Host 'Initialized missing or blank credentials in ops/.env.'
    }
}

function Get-KamuiConfig {
    if (-not [IO.File]::Exists($script:EnvPath)) { throw 'Run start-kamui.ps1 to initialize ops/.env.' }
    $parsed = (Invoke-KamuiCompose config --format json | Out-String) | ConvertFrom-Json
    $console = $parsed.services.console
    $token = $console.environment.KAMUI_TOKEN
    $password = $console.environment.KAMUI_ADMIN_PASSWORD
    if ($token.Length -lt 32 -or $token -match '\s|[\x00-\x1f\x7f]' -or
        $password.Length -lt 24 -or $password -match '[\x00-\x1f\x7f]' -or $password -ceq $token) {
        throw 'Set distinct strong credentials in ops/.env. Use 32 random bytes per credential (hex).'
    }

    $proxy = $parsed.services.'workspace-proxy'
    $binding = $proxy.ports[0]
    $bindIP = $binding.host_ip
    $parsedIP = $null
    if (-not [Net.IPAddress]::TryParse($bindIP, [ref]$parsedIP) -or $parsedIP.ToString() -cne $bindIP) {
        throw 'KAMUI_BIND_IP must be a canonical private IPv4 address or 127.0.0.1.'
    }
    $octets = $parsedIP.GetAddressBytes()
    $private = $octets.Length -eq 4 -and ($octets[0] -eq 10 -or
        ($octets[0] -eq 172 -and $octets[1] -ge 16 -and $octets[1] -le 31) -or
        ($octets[0] -eq 192 -and $octets[1] -eq 168))
    if ($bindIP -ne '127.0.0.1' -and -not $private) { throw 'Use loopback or one private LAN IPv4 address.' }

    $port = [int]$binding.published
    if ($port -lt 1 -or $port -gt 65535) { throw 'Set KAMUI_PORT to a valid TCP port.' }
    $servingPath = $proxy.volumes[0].source
    if ($env:OS -eq 'Windows_NT' -and $servingPath -match '^/run/desktop/mnt/host/([a-z])/(.*)$') {
        $servingPath = "${Matches[1]}:/${Matches[2]}"
    }
    return @{
        Project = $parsed.name
        BindIP = $bindIP
        Port = $port
        Url = "https://${bindIP}:$port"
        CA = Join-Path (Split-Path -Parent $servingPath) 'ca.pem'
    }
}

function Assert-KamuiPort {
    param($Config)

    $proxyId = Invoke-KamuiCompose ps -q workspace-proxy
    if ($proxyId) {
        $published = Invoke-KamuiDocker port $proxyId 443/tcp
        if ($published -contains "$($Config.BindIP):$($Config.Port)") { return }
    }
    if ($env:OS -eq 'Windows_NT' -and $Config.BindIP -ne '127.0.0.1') {
        $lan = Get-NetAdapter -Physical | Where-Object Status -eq 'Up' |
            Get-NetIPAddress -AddressFamily IPv4
        if ($lan.IPAddress -notcontains $Config.BindIP) {
            throw 'KAMUI_BIND_IP does not belong to an active physical interface. Check Get-NetIPConfiguration.'
        }
    }

    $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Parse($Config.BindIP), $Config.Port)
    $listener.ExclusiveAddressUse = $true
    try { $listener.Start() }
    catch { throw "Cannot bind $($Config.BindIP):$($Config.Port). Resolve the port conflict or choose KAMUI_PORT." }
    finally { $listener.Stop() }
}

function Invoke-KamuiProbe {
    param($Config, [string]$Path, [string]$Expected, [string[]]$Headers = @())

    $curl = Get-Command curl.exe -ErrorAction SilentlyContinue
    if (-not $curl -and $env:OS -ne 'Windows_NT') { $curl = Get-Command curl -ErrorAction SilentlyContinue }
    if (-not $curl) { throw 'curl.exe is needed to verify HTTPS with the local CA.' }
    if (-not (Test-Path -LiteralPath $Config.CA)) { throw "TLS CA missing at $($Config.CA)." }

    $curlArgs = @('--silent', '--show-error', '--max-time', '10', '--http1.1', '--cacert', $Config.CA,
        '--output', $(if ($env:OS -eq 'Windows_NT') { 'NUL' } else { '/dev/null' }), '--write-out', '%{http_code}')
    foreach ($header in $Headers) { $curlArgs += @('--header', $header) }
    $actual = & $curl.Source @curlArgs "$($Config.Url)$Path"
    if ($LASTEXITCODE -ne 0 -or $actual -ne $Expected) {
        throw "HTTPS probe for $Path expected $Expected and received $actual at $($Config.Url)."
    }
}

function Assert-KamuiReady {
    param($Config)

    foreach ($service in $script:KamuiServices) {
        $id = Invoke-KamuiCompose ps -q $service
        if (-not $id -or (Invoke-KamuiDocker inspect --format '{{.State.Health.Status}}' $id) -ne 'healthy') {
            throw "$service is unhealthy or stopped. Use docker compose logs for that service."
        }
    }
    Invoke-KamuiProbe $Config '/healthz' '200'
    Invoke-KamuiProbe $Config '/api/workspaces' '401'
}
