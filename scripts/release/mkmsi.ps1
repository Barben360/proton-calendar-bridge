<#
.SYNOPSIS
Builds pcb_windows_amd64.msi from pcb.exe and pcb-ui.exe (packaging/windows/pcb.wxs).

.DESCRIPTION
Needs WiX Toolset 7.0.1 (a .NET 8+ tool) and its UI and Util extensions:

    dotnet tool install --global wix --version 7.0.1
    wix extension add -g WixToolset.UI.wixext/7.0.1 WixToolset.Util.wixext/7.0.1 -acceptEula wix7

Every wix call passes -acceptEula wix7: the author accepted WiX v7's Open
Source Maintenance Fee EULA (2026-11-15). Version is the MSI's
ProductVersion, X.Y.Z (scripts/release/check.sh msi-version TAG).

-Validate then runs Windows Installer's ICE checks (wix msi validate): an
ICE error fails the build, warnings are printed (ICE61: same-version
upgrades are allowed; ICE91: a per-user package). They need the Windows
Installer service, which an SSH session of a standard user cannot reach.

Exits 1 when WiX reports an error or the MSI is not written, 2 for a bad
argument.

.EXAMPLE
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\release\mkmsi.ps1 -Version 0.2.0 -Pcb bin\pcb.exe -PcbUi bin\pcb-ui.exe -Out bin\pcb_windows_amd64.msi -Validate
#>
param(
    [Parameter(Mandatory = $true)] [string] $Version,
    [Parameter(Mandatory = $true)] [string] $Pcb,
    [Parameter(Mandatory = $true)] [string] $PcbUi,
    [Parameter(Mandatory = $true)] [string] $Out,
    [switch] $Validate
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$WixVersion = '7.0.1'

function Refuse([string] $why) { [Console]::Error.WriteLine("mkmsi: $why"); exit 2 }

if ($Version -notmatch '^(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,4})$' -or
    [int]$Matches[1] -gt 255 -or [int]$Matches[2] -gt 255 -or [int]$Matches[3] -gt 65535) {
    Refuse "-Version '$Version' is not an MSI version X.Y.Z (at most 255.255.65535)"
}
foreach ($f in @($Pcb, $PcbUi)) {
    if (-not (Test-Path -LiteralPath $f -PathType Leaf)) { Refuse "no file $f" }
}
$wix = Get-Command wix -ErrorAction SilentlyContinue
if (-not $wix) { Refuse "no wix on PATH: dotnet tool install --global wix --version $WixVersion" }
$got = (& wix --version -acceptEula wix7 | Out-String).Trim()
if ($got -notlike "$WixVersion*") { Refuse "wix is $got, want $WixVersion" }

$src = Join-Path $PSScriptRoot '..\..\packaging\windows\pcb.wxs'
$outFull = [IO.Path]::GetFullPath($Out)
$outDir = Split-Path -Parent $outFull
if (-not (Test-Path -LiteralPath $outDir)) { New-Item -ItemType Directory -Path $outDir | Out-Null }
if (Test-Path -LiteralPath $outFull) { Remove-Item -LiteralPath $outFull }
$tmp = Join-Path ([IO.Path]::GetTempPath()) ('pcb-mkmsi-' + [Guid]::NewGuid().ToString('N').Substring(0, 8))

& wix build -acceptEula wix7 -nologo -arch x64 `
    -ext "WixToolset.UI.wixext/$WixVersion" -ext "WixToolset.Util.wixext/$WixVersion" `
    -d "Version=$Version" `
    -d "PcbExe=$((Resolve-Path -LiteralPath $Pcb).Path)" `
    -d "PcbUiExe=$((Resolve-Path -LiteralPath $PcbUi).Path)" `
    -intermediatefolder $tmp -pdbtype none `
    -o $outFull $src
$code = $LASTEXITCODE
Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
if ($code -ne 0) { [Console]::Error.WriteLine("mkmsi: wix build exited $code"); exit 1 }
if (-not (Test-Path -LiteralPath $outFull -PathType Leaf)) { [Console]::Error.WriteLine("mkmsi: wix wrote no $outFull"); exit 1 }
if ($Validate) {
    & wix msi validate -acceptEula wix7 -nologo $outFull
    if ($LASTEXITCODE -ne 0) { [Console]::Error.WriteLine("mkmsi: wix msi validate exited $LASTEXITCODE"); exit 1 }
}
Write-Output "mkmsi: $outFull ($Version)"
