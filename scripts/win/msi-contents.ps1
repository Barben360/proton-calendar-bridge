<#
.SYNOPSIS
Checks that pcb_windows_amd64.msi holds exactly the pcb.exe and pcb-ui.exe of a directory.

.DESCRIPTION
An administrative install (msiexec /a) unpacks the MSI's files into a
scratch directory without installing anything. The MSI must hold exactly
pcb.exe and pcb-ui.exe, each byte for byte the file of the same name in
-Dir. CI compares against bin\ (the binaries it built), the release
against the zip's (the release's own binaries).

Prints each unpacked file with its SHA-256, then "msi-contents: ok"; on a
mismatch prints "msi-contents: FAIL <why>" and exits 1 (2 for a bad
argument).

.EXAMPLE
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\win\msi-contents.ps1 -Msi bin\pcb_windows_amd64.msi -Dir bin
#>
param(
    [Parameter(Mandatory = $true)] [string] $Msi,
    [Parameter(Mandatory = $true)] [string] $Dir
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$Want = @('pcb.exe', 'pcb-ui.exe')

function Refuse([string] $why) { [Console]::Error.WriteLine("msi-contents: $why"); exit 2 }
function Fail([string] $why) { Write-Output "msi-contents: FAIL $why"; exit 1 }

if (-not (Test-Path -LiteralPath $Msi -PathType Leaf)) { Refuse "no file $Msi" }
foreach ($n in $Want) {
    if (-not (Test-Path -LiteralPath (Join-Path $Dir $n) -PathType Leaf)) { Refuse "no file $(Join-Path $Dir $n)" }
}
$Msi = (Resolve-Path -LiteralPath $Msi).Path
$work = Join-Path ([IO.Path]::GetTempPath()) ('pcb-msi-contents-' + [Guid]::NewGuid().ToString('N').Substring(0, 8))
$target = Join-Path $work 'files'
$log = Join-Path $work 'admin.log'
New-Item -ItemType Directory -Path $work | Out-Null
try {
    $p = Start-Process -FilePath msiexec.exe -ArgumentList @('/a', "`"$Msi`"", '/qn', "TARGETDIR=`"$target`"", '/l*v', "`"$log`"") -Wait -PassThru
    if ($p.ExitCode -ne 0) {
        if (Test-Path -LiteralPath $log) { Get-Content -LiteralPath $log -Tail 40 }
        Fail "msiexec /a exited $($p.ExitCode)"
    }
    # The administrative image holds a copy of the MSI beside the files.
    $files = @(Get-ChildItem -LiteralPath $target -Recurse -File | Where-Object Extension -ne '.msi')
    foreach ($f in $files) { Write-Output "$($f.FullName) $((Get-FileHash -LiteralPath $f.FullName).Hash)" }
    $names = ($files | ForEach-Object Name | Sort-Object) -join ','
    $wantNames = ($Want | Sort-Object) -join ','
    if ($names -ne $wantNames) { Fail "the MSI holds [$names], want [$wantNames]" }
    foreach ($f in $files) {
        $wantHash = (Get-FileHash -LiteralPath (Join-Path $Dir $f.Name)).Hash
        if ((Get-FileHash -LiteralPath $f.FullName).Hash -ne $wantHash) { Fail "the MSI's $($f.Name) is not $(Join-Path $Dir $f.Name)" }
    }
} finally {
    Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
}
Write-Output "msi-contents: ok, the MSI holds $Dir's pcb.exe and pcb-ui.exe"
