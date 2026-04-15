param(
    [string]$ConfigPath = "config-handao-dev.yaml",
    [string]$RemoteDir = "oss-sync",
    [string]$BinaryName = "oss-sync-linux-amd64"
)

$ErrorActionPreference = "Stop"

$projectRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
$distDir = Join-Path $projectRoot "dist"
$binaryPath = Join-Path $distDir $BinaryName
$configFullPath = Join-Path $projectRoot $ConfigPath

if (-not (Test-Path $distDir)) {
    New-Item -ItemType Directory -Path $distDir | Out-Null
}
if (-not (Test-Path $configFullPath)) {
    throw "Config file not found: $configFullPath"
}

Push-Location $projectRoot
try {
    $env:GOOS = "linux"
    $env:GOARCH = "amd64"
    $env:CGO_ENABLED = "0"

    go build -trimpath -ldflags="-s -w" -o $binaryPath .\cmd
    if ($LASTEXITCODE -ne 0) {
        throw "go build failed"
    }

    & ossfs upload $binaryPath "$RemoteDir/$BinaryName"
    if ($LASTEXITCODE -ne 0) {
        throw "binary upload failed"
    }

    & ossfs upload $configFullPath "$RemoteDir/$(Split-Path $ConfigPath -Leaf)"
    if ($LASTEXITCODE -ne 0) {
        throw "config upload failed"
    }

    & ossfs ls "$RemoteDir/"
}
finally {
    Pop-Location
}
