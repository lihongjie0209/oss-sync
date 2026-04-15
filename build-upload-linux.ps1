param(
    [string]$ConfigPattern = "config-*.yaml",
    [string]$RemoteDir = "oss-sync",
    [string]$BinaryName = "oss-sync-linux-amd64"
)

$ErrorActionPreference = "Stop"

$projectRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
$distDir = Join-Path $projectRoot "dist"
$binaryPath = Join-Path $distDir $BinaryName
$configFiles = Get-ChildItem -Path $projectRoot -Filter $ConfigPattern -File

if (-not (Test-Path $distDir)) {
    New-Item -ItemType Directory -Path $distDir | Out-Null
}
if (-not $configFiles -or $configFiles.Count -eq 0) {
    throw "No config files found with pattern '$ConfigPattern' under: $projectRoot"
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

    foreach ($configFile in $configFiles) {
        & ossfs upload $configFile.FullName "$RemoteDir/$($configFile.Name)"
        if ($LASTEXITCODE -ne 0) {
            throw "config upload failed: $($configFile.FullName)"
        }
    }

    & ossfs ls "$RemoteDir/"
}
finally {
    Pop-Location
}
