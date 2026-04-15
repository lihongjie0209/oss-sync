param(
    [string]$BinaryPrefix = "oss-sync",
    [string]$OutputDir = "dist"
)

$ErrorActionPreference = "Stop"

$projectRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
$distDir = Join-Path $projectRoot $OutputDir

if (-not (Test-Path $distDir)) {
    New-Item -ItemType Directory -Path $distDir | Out-Null
}

$targets = @(
    @{ GOOS = "windows"; GOARCH = "amd64"; Name = "$BinaryPrefix-windows-amd64.exe" },
    @{ GOOS = "linux"; GOARCH = "amd64"; Name = "$BinaryPrefix-linux-amd64" },
    @{ GOOS = "darwin"; GOARCH = "amd64"; Name = "$BinaryPrefix-darwin-amd64" }
)

$originalGoos = $env:GOOS
$originalGoarch = $env:GOARCH
$originalCgoEnabled = $env:CGO_ENABLED

Push-Location $projectRoot
try {
    foreach ($target in $targets) {
        $env:GOOS = $target.GOOS
        $env:GOARCH = $target.GOARCH
        $env:CGO_ENABLED = "0"

        $outputPath = Join-Path $distDir $target.Name
        Write-Host "Building $($target.GOOS)/$($target.GOARCH) -> $outputPath"

        go build -trimpath -ldflags="-s -w" -o $outputPath .\cmd
        if ($LASTEXITCODE -ne 0) {
            throw "go build failed for $($target.GOOS)/$($target.GOARCH)"
        }
    }

    Write-Host ""
    Write-Host "Build outputs:"
    Get-ChildItem -Path $distDir | Sort-Object Name | Select-Object Name, Length, LastWriteTime
}
finally {
    if ($null -eq $originalGoos) {
        Remove-Item Env:GOOS -ErrorAction SilentlyContinue
    } else {
        $env:GOOS = $originalGoos
    }

    if ($null -eq $originalGoarch) {
        Remove-Item Env:GOARCH -ErrorAction SilentlyContinue
    } else {
        $env:GOARCH = $originalGoarch
    }

    if ($null -eq $originalCgoEnabled) {
        Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue
    } else {
        $env:CGO_ENABLED = $originalCgoEnabled
    }

    Pop-Location
}
