# Build breakwater-agent.msi with WiX v5 on Windows.
# Invoked by CI (windows-latest). Validates the toolchain on first real run.
#
# Prerequisites:
#   - breakwater-agent.exe already built (GOOS=windows)
#   - WiX v5 CLI on PATH (`wix`) OR install via:
#       dotnet tool install --global wix --version 5.0.2
#       wix extension add WixToolset.Util.wixext
#
# Usage:
#   .\build.ps1 -AgentExe .\breakwater-agent.exe -Version 0.0.1 -OutDir .\out

param(
    [Parameter(Mandatory = $true)][string]$AgentExe,
    [string]$Version = "0.0.1",
    [string]$OutDir = ".\out"
)

$ErrorActionPreference = "Stop"

if (-not (Test-Path $AgentExe)) {
    throw "AgentExe not found: $AgentExe"
}

New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$msi = Join-Path $OutDir "breakwater-agent.msi"
$wxs = Join-Path $PSScriptRoot "BreakwaterAgent.wxs"

Write-Host "Building MSI with WiX v5…"
Write-Host "  AgentExe=$AgentExe"
Write-Host "  Version=$Version"
Write-Host "  Out=$msi"

# Prefer `wix` CLI (WiX v5). Fall back to candle/light message if missing.
$wix = Get-Command wix -ErrorAction SilentlyContinue
if (-not $wix) {
    Write-Host "WiX CLI not on PATH; attempting dotnet tool install…"
    dotnet tool install --global wix --version 5.0.2
    $env:PATH = "$env:USERPROFILE\.dotnet\tools;$env:PATH"
    $wix = Get-Command wix -ErrorAction SilentlyContinue
}
if (-not $wix) {
    throw "WiX v5 CLI (`wix`) not available. Install: dotnet tool install --global wix --version 5.0.2"
}

& wix build `
    -o $msi `
    -d "AgentExePath=$AgentExe" `
    -d "ProductVersion=$Version" `
    $wxs

if ($LASTEXITCODE -ne 0) {
    throw "wix build failed with exit $LASTEXITCODE"
}

# SHA256 for unsigned MVP distribution.
$hash = (Get-FileHash -Algorithm SHA256 $msi).Hash.ToLower()
$hashFile = "$msi.sha256"
Set-Content -Path $hashFile -Value "$hash  $(Split-Path $msi -Leaf)`n" -NoNewline
Write-Host "MSI: $msi"
Write-Host "SHA256: $hash"
Write-Host "Wrote $hashFile"
