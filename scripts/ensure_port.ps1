param(
    [Parameter(Mandatory = $true)]
    [string]$ConfigPath
)

# Tra ve port trong (chua bi chiem) qua stdout.
# Neu port trong config dang bi chiem, tim port trong ke tiep va ghi lai vao config.json.
# Moi thong bao trung gian in ra stderr de khong lam ban gia tri port o stdout.

$ErrorActionPreference = 'Stop'

function Test-PortInUse([int]$Port) {
    $listeners = [System.Net.NetworkInformation.IPGlobalProperties]::GetIPGlobalProperties().GetActiveTcpListeners()
    foreach ($l in $listeners) {
        if ($l.Port -eq $Port) { return $true }
    }
    return $false
}

# Doc config JSON
$raw = Get-Content -Raw -Path $ConfigPath -Encoding UTF8
$config = $raw | ConvertFrom-Json

$startPort = 8080
if ($config.PSObject.Properties.Name -contains 'port' -and $config.port) {
    $startPort = [int]$config.port
}

$port = $startPort
$maxPort = $startPort + 50
$changed = $false

while ((Test-PortInUse -Port $port) -and ($port -le $maxPort)) {
    Write-Host "[!] Port $port dang bi chiem, thu port $($port + 1)..." -ForegroundColor Yellow
    $port++
    $changed = $true
}

if ($port -gt $maxPort) {
    Write-Error "Khong tim duoc port trong trong khoang $startPort-$maxPort"
    exit 1
}

# Neu port thay doi, ghi lai vao config.json (giu nguyen dinh dang, chi doi truong port)
if ($changed) {
    $config.port = $port
    $json = $config | ConvertTo-Json -Depth 100
    # Ghi UTF8 khong BOM
    $utf8NoBom = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText((Resolve-Path $ConfigPath), $json, $utf8NoBom)
    Write-Host "[i] Da cap nhat port trong $ConfigPath thanh $port" -ForegroundColor Cyan
}

# Chi dong duy nhat nay ra stdout: gia tri port
Write-Output $port
