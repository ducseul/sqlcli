# Builds stripped single-binary releases for common platforms into ./dist
param(
    [string]$Version = "0.1.0",
    [string[]]$Targets = @("windows/amd64", "linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64")
)

$ErrorActionPreference = "Stop"
New-Item -ItemType Directory -Force dist | Out-Null

foreach ($t in $Targets) {
    $os, $arch = $t.Split("/")
    $ext = if ($os -eq "windows") { ".exe" } else { "" }
    $out = "dist/sqlcli-$os-$arch$ext"
    Write-Host "building $out"
    $env:GOOS = $os; $env:GOARCH = $arch; $env:CGO_ENABLED = "0"
    go build -trimpath -ldflags "-s -w -X main.version=$Version" -o $out .
}
Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
Get-ChildItem dist | ForEach-Object { "{0,-28} {1,6:N1} MB" -f $_.Name, ($_.Length / 1MB) }
