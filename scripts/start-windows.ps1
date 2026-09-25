param(
    [switch]$NoBrowser
)

$ErrorActionPreference = 'Stop'
$installDir = Split-Path -Parent $PSCommandPath
$exePath = Join-Path $installDir 'doal.exe'
$dataDir = Join-Path $env:LOCALAPPDATA 'DOAL'
$url = 'http://127.0.0.1:5082/doal/ui/'
$port = 5082

function Show-StartupError([string]$message) {
    $logPath = Join-Path $dataDir 'launcher-error.log'
    Add-Content -LiteralPath $logPath -Value "$(Get-Date -Format o) $message"
    Add-Type -AssemblyName System.Windows.Forms
    [void][System.Windows.Forms.MessageBox]::Show(
        "$message`n`nDetails: $logPath",
        'DOAL - startup error',
        [System.Windows.Forms.MessageBoxButtons]::OK,
        [System.Windows.Forms.MessageBoxIcon]::Error
    )
}

try {
    if (-not (Test-Path -LiteralPath $exePath -PathType Leaf)) {
        throw "DOAL executable is missing: $exePath"
    }
    if (-not (Test-Path -LiteralPath (Join-Path $dataDir 'config.json') -PathType Leaf)) {
        throw "DOAL configuration is missing: $dataDir\config.json"
    }

    # Explorer can retain an old environment after a user variable changes.
    $token = [Environment]::GetEnvironmentVariable('DOAL_SECRET_TOKEN', 'User')
    if ([string]::IsNullOrWhiteSpace($token)) {
        $token = [Environment]::GetEnvironmentVariable('DOAL_SECRET_TOKEN', 'Process')
    }
    if ([string]::IsNullOrWhiteSpace($token) -or ($token -ne 'x' -and $token.Length -lt 32)) {
        throw 'DOAL_SECRET_TOKEN is missing or too short in the user environment.'
    }

    $running = @(Get-Process -Name 'doal' -ErrorAction SilentlyContinue | Where-Object {
        $_.Path -and [string]::Equals($_.Path, $exePath, [StringComparison]::OrdinalIgnoreCase)
    })
    $listener = Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue |
        Where-Object { $_.LocalAddress -eq '127.0.0.1' } |
        Select-Object -First 1

    if ($listener -and -not ($running | Where-Object { $_.Id -eq $listener.OwningProcess })) {
        throw "Port $port is already used by another process (PID $($listener.OwningProcess))."
    }

    if (-not $running) {
        $env:DOAL_SECRET_TOKEN = $token
        Start-Process -FilePath $exePath -ArgumentList @(
            "--conf=$dataDir", "--port=$port", '--path-prefix=doal'
        ) -WorkingDirectory $dataDir -WindowStyle Hidden `
            -RedirectStandardOutput (Join-Path $dataDir 'doal.stdout.log') `
            -RedirectStandardError (Join-Path $dataDir 'doal.stderr.log') | Out-Null
    }

    $ready = $false
    for ($attempt = 0; $attempt -lt 30; $attempt++) {
        try {
            $response = Invoke-WebRequest -Uri $url -UseBasicParsing -TimeoutSec 2
            if ($response.StatusCode -eq 200 -and $response.Content -match '<title>.*DOAL') {
                $ready = $true
                break
            }
        } catch {
            Start-Sleep -Milliseconds 500
        }
    }
    if (-not $ready) {
        throw "DOAL did not serve its interface on $url. Check doal.stderr.log in $dataDir."
    }

    if (-not $NoBrowser) {
        Start-Process -FilePath $url | Out-Null
    }
} catch {
    Show-StartupError $_.Exception.Message
    exit 1
}
