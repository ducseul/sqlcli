# Builds stripped single-binary releases for common platforms into ./dist
# Every run bumps the patch number in ./VERSION (0.1.0 -> 0.1.1) unless -Version is given.
param(
    [string]$Version = "",
    [string[]]$Targets = @("windows/amd64", "linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64")
)

$ErrorActionPreference = "Stop"
if (-not $Version) {
    $cur = (Get-Content VERSION -ErrorAction SilentlyContinue | Select-Object -First 1).Trim()
    if ($cur -notmatch '^(\d+)\.(\d+)\.(\d+)$') { $cur = "0.1.0" }
    $Version = "{0}.{1}.{2}" -f $Matches[1], $Matches[2], ([int]$Matches[3] + 1)
}
Set-Content -Path VERSION -Value $Version -Encoding ascii
Write-Host "version $Version"
New-Item -ItemType Directory -Force dist | Out-Null
$buildDate = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")

foreach ($t in $Targets) {
    $os, $arch = $t.Split("/")
    $ext = if ($os -eq "windows") { ".exe" } else { "" }
    $out = "dist/sqlcli-$os-$arch$ext"
    Write-Host "building $out"
    $env:GOOS = $os; $env:GOARCH = $arch; $env:CGO_ENABLED = "0"
    go build -trimpath -ldflags "-s -w -X main.version=$Version -X main.buildDate=$buildDate" -o $out .
}
Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
Get-ChildItem dist | ForEach-Object { "{0,-28} {1,6:N1} MB" -f $_.Name, ($_.Length / 1MB) }
