<#
.SYNOPSIS
Installs WiX Toolset 7.0.1 and its UI and Util extensions for the current user.

.DESCRIPTION
The tools CI's windows job and the release's msi job build the MSI with
(decision 0019): `dotnet tool install --global wix --version 7.0.1`, then
the two extensions at 7.0.1. Every wix command passes -acceptEula wix7:
the author accepted WiX 7's Open Source Maintenance Fee EULA (2026-11-15).
Needs a .NET SDK 8 or later on PATH. On a runner (GITHUB_PATH set), the
.NET tools directory is added to the PATH of the later steps.

Exits 1 when a command fails or wix is not 7.0.1.

.EXAMPLE
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\win\install-wix.ps1
#>
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$WixVersion = '7.0.1'

function Run([string] $what, [scriptblock] $block) {
    & $block
    if ($LASTEXITCODE -ne 0) { [Console]::Error.WriteLine("install-wix: $what exited $LASTEXITCODE"); exit 1 }
}

Run 'dotnet --list-sdks' { dotnet --list-sdks }
Run 'dotnet tool install' { dotnet tool install --global wix --version $WixVersion }
$tools = Join-Path $env:USERPROFILE '.dotnet\tools'
$env:PATH = "$tools;$env:PATH"
Run 'wix extension add' { wix extension add -g "WixToolset.UI.wixext/$WixVersion" "WixToolset.Util.wixext/$WixVersion" -acceptEula wix7 }
$got = (& wix --version -acceptEula wix7 | Out-String).Trim()
# 7.0.1+<commit>, not 7.0.10.
if ($got -notmatch ('^' + [regex]::Escape($WixVersion) + '(\+|$)')) { [Console]::Error.WriteLine("install-wix: wix is $got, want $WixVersion"); exit 1 }
Write-Output "install-wix: wix $got"
if ($env:GITHUB_PATH) { Add-Content -LiteralPath $env:GITHUB_PATH -Value $tools }
