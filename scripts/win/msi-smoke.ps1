<#
.SYNOPSIS
Smoke test of pcb_windows_amd64.msi (spec/designs/2026-11-15-windows-msi-design.md §5).

.DESCRIPTION
Installs the MSI silently for the current user, checks what it installed,
changes its features, repairs it and uninstalls it, printing "ok   <check>"
for each check and PASS last. The first failed check prints
"FAIL <check>: <why>" and the script exits 1 (2 for a usage error).

It changes the current user's PATH and Run key for the run and puts both
back at the end, uninstalls what it installed, and leaves a marker file in
%APPDATA%\pcb and %LOCALAPPDATA%\pcb only while it runs. pcb must not be
installed from an MSI when it starts.

-UpgradeMsi, a second build of the same sources (another ProductCode, the
same UpgradeCode), adds the major-upgrade checks.

.EXAMPLE
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\win\msi-smoke.ps1 -Msi bin\pcb_windows_amd64.msi -Version 0.2.0
#>
param(
    [Parameter(Mandatory = $true)] [string] $Msi,
    [string] $Version,
    [string] $UpgradeMsi
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$InstallDir = Join-Path $env:LOCALAPPDATA 'Programs\pcb'
$UiExe = Join-Path $InstallDir 'pcb-ui.exe'
$CliExe = Join-Path $InstallDir 'pcb.exe'
$Shortcut = Join-Path $env:APPDATA 'Microsoft\Windows\Start Menu\Programs\pcb-ui.lnk'
$AppID = 'io.github.barben360.pcb-ui'
$RunKey = 'Software\Microsoft\Windows\CurrentVersion\Run'
$RunData = '"' + $UiExe + '" --background' # internal/runkey.Data
$Elsewhere = '"C:\pcb-msi-smoke-elsewhere\pcb-ui.exe" --background'
$OwnKey = 'Software\proton-calendar-bridge'
# Other PATH entries the MSI must leave as they are: a variable that stays
# unexpanded, and enough length that a 1024-character cut would show.
$OtherEntries = @('%USERPROFILE%\pcb-msi-smoke-expand') + (1..12 | ForEach-Object { 'C:\pcb-msi-smoke-other-' + ('x' * 90) + "-$_" })
$Markers = @((Join-Path $env:APPDATA 'pcb'), (Join-Path $env:LOCALAPPDATA 'pcb'))

$script:check = 'arguments'

function Fail([string] $why) {
    Write-Output "FAIL $($script:check): $why"
    throw [System.OperationCanceledException]::new('check failed')
}

function Pass { Write-Output "ok   $($script:check)" }

# Check NAME BLOCK: BLOCK returns '' when the check holds, else why it fails.
function Check([string] $name, [scriptblock] $block) {
    $script:check = $name
    $why = & $block
    if ($why) { Fail $why }
    Pass
}

# --- Windows Installer ---------------------------------------------------

function Invoke-Com($obj, [string] $member, [string] $kind, [object[]] $arguments) {
    $flags = [System.Reflection.BindingFlags]::$kind
    return $obj.GetType().InvokeMember($member, $flags, $null, $obj, $arguments)
}

$Installer = New-Object -ComObject WindowsInstaller.Installer

function Get-MsiProperty([string] $path, [string] $name) {
    $db = Invoke-Com $Installer 'OpenDatabase' InvokeMethod @($path, 0)
    $view = Invoke-Com $db 'OpenView' InvokeMethod @("SELECT ``Value`` FROM ``Property`` WHERE ``Property`` = '$name'")
    [void](Invoke-Com $view 'Execute' InvokeMethod @())
    $rec = Invoke-Com $view 'Fetch' InvokeMethod @()
    $value = if ($rec) { Invoke-Com $rec 'StringData' GetProperty @(1) } else { '' }
    [void](Invoke-Com $view 'Close' InvokeMethod @())
    foreach ($o in @($rec, $view, $db)) { if ($o) { [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($o) } }
    return $value
}

function Get-RelatedProducts([string] $upgradeCode) {
    $list = Invoke-Com $Installer 'RelatedProducts' GetProperty @($upgradeCode)
    $out = @()
    foreach ($p in $list) { $out += $p }
    return , $out
}

function Test-Installed([string] $productCode) {
    return (Invoke-Com $Installer 'ProductState' GetProperty @($productCode)) -eq 5
}

$Work = Join-Path ([IO.Path]::GetTempPath()) ('pcb-msi-smoke-' + [Guid]::NewGuid().ToString('N').Substring(0, 8))
$script:step = 0

# Msiexec NAME ARGS: runs msiexec /qn with a verbose log; NAME fails unless
# it exits 0 (3010 and 1641 ask for a reboot: a file was in use).
function Msiexec([string] $name, [string[]] $arguments) {
    $script:check = $name
    $script:step++
    $log = Join-Path $Work ("{0:D2}.log" -f $script:step)
    $script:lastLog = $log
    $all = $arguments + @('/qn', '/norestart', '/l*v', "`"$log`"")
    $p = Start-Process -FilePath msiexec.exe -ArgumentList $all -Wait -PassThru
    if ($p.ExitCode -ne 0) {
        $tail = if (Test-Path $log) { (Get-Content $log | Select-String -Pattern 'error|value 3' | Select-Object -Last 8) -join "`n" } else { '(no log)' }
        Fail "msiexec $($arguments -join ' ') exited $($p.ExitCode); log $log`n$tail"
    }
    Pass
}

# MsiexecRefused NAME ARGS: msiexec /qn must fail with 1603 (a fatal
# error) and leave nothing installed.
function MsiexecRefused([string] $name, [string[]] $arguments) {
    $script:check = $name
    $script:step++
    $log = Join-Path $Work ("{0:D2}.log" -f $script:step)
    $p = Start-Process -FilePath msiexec.exe -ArgumentList ($arguments + @('/qn', '/norestart', '/l*v', "`"$log`"")) -Wait -PassThru
    if ($p.ExitCode -ne 1603) { Fail "msiexec $($arguments -join ' ') exited $($p.ExitCode), want 1603; log $log" }
    $w = Why-Uninstalled
    if ($w) { Fail $w }
    Pass
}

# --- the current user's registry -----------------------------------------

function Get-Value([string] $key, [string] $name) {
    $k = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey($key)
    if (-not $k) { return $null }
    try {
        $v = $k.GetValue($name, $null, [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
        if ($null -eq $v) { return $null }
        return [pscustomobject]@{ Data = $v; Kind = $k.GetValueKind($name) }
    } finally { $k.Close() }
}

function Set-Value([string] $key, [string] $name, $value) {
    $k = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey($key)
    try {
        if ($null -eq $value) { $k.DeleteValue($name, $false) } else { $k.SetValue($name, $value.Data, $value.Kind) }
    } finally { $k.Close() }
}

function Get-UserPath { return Get-Value 'Environment' 'Path' }

function Set-UserPath([string[]] $entries) {
    Set-Value 'Environment' 'Path' ([pscustomobject]@{ Data = ($entries -join ';'); Kind = [Microsoft.Win32.RegistryValueKind]::ExpandString })
}

function Get-Run { return Get-Value $RunKey 'pcb-ui' }

function Set-Run([string] $data) {
    $v = if ($data) { [pscustomobject]@{ Data = $data; Kind = [Microsoft.Win32.RegistryValueKind]::String } } else { $null }
    Set-Value $RunKey 'pcb-ui' $v
}

function Split-Path-Entries([string] $path) { return , @($path -split ';' | Where-Object { $_ -ne '' }) }

function Same-Dir([string] $a, [string] $b) { return $a.TrimEnd('\') -ieq $b.TrimEnd('\') }

# Why-* functions return '' when the state holds, else why not.

function Why-PathEntry([int] $want, [string[]] $others) {
    $v = Get-UserPath
    if (-not $v) { return 'HKCU\Environment has no Path value' }
    if ($v.Kind -ne 'ExpandString') { return "Path is $($v.Kind), want ExpandString (its %VARIABLES% would stop expanding)" }
    $entries = Split-Path-Entries $v.Data
    $n = @($entries | Where-Object { Same-Dir $_ $InstallDir }).Count
    if ($n -ne $want) { return "Path holds $InstallDir $n times, want $want`: $($v.Data)" }
    $rest = @($entries | Where-Object { -not (Same-Dir $_ $InstallDir) })
    if (($rest -join ';') -cne ($others -join ';')) { return "the other entries changed: $($rest -join ';') (want $($others -join ';'))" }
    return ''
}

function Why-Run([string] $want) {
    $v = Get-Run
    if (-not $want) { if ($v) { return "the Run value pcb-ui is present: $($v.Data)" } else { return '' } }
    if (-not $v) { return 'no Run value pcb-ui' }
    if ($v.Kind -ne 'String') { return "the Run value is $($v.Kind), want String" }
    if ($v.Data -cne $want) { return "the Run value is [$($v.Data)], want [$want]" }
    return ''
}

# Why-PropertyUnset NAME: the last msiexec log's closing property dump
# (Property(S): ...) has no NAME.
function Why-PropertyUnset([string] $name) {
    $dump = @(Select-String -LiteralPath $script:lastLog -Pattern '^Property\(S\): ' | ForEach-Object { $_.Line })
    if (-not ($dump -match '^Property\(S\): ProductCode = ')) { return "no property dump in $($script:lastLog)" }
    $set = @($dump -match ('^Property\(S\): ' + [regex]::Escape($name) + ' = '))
    if ($set.Count) { return "$($set -join '; ') (log $($script:lastLog))" }
    return ''
}

function Why-Files([string[]] $want) {
    if (-not (Test-Path -LiteralPath $InstallDir -PathType Container)) { return "no $InstallDir" }
    $got = @(Get-ChildItem -LiteralPath $InstallDir -Force | ForEach-Object { $_.Name } | Sort-Object)
    if (($got -join ',') -ne (($want | Sort-Object) -join ',')) { return "$InstallDir holds [$($got -join ', ')], want [$($want -join ', ')]" }
    return ''
}

# --- the shortcut --------------------------------------------------------

Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;

public static class PcbSmokeShell {
    [StructLayout(LayoutKind.Sequential, Pack = 4)]
    public struct PropertyKey { public Guid fmtid; public uint pid; }

    [StructLayout(LayoutKind.Explicit, Size = 24)]
    public struct PropVariant { [FieldOffset(0)] public ushort vt; [FieldOffset(8)] public IntPtr p; }

    [ComImport, Guid("886D8EEB-8CF2-4446-8D02-CDBA1DBDCF99"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
    public interface IPropertyStore {
        [PreserveSig] int GetCount(out uint count);
        [PreserveSig] int GetAt(uint index, out PropertyKey key);
        [PreserveSig] int GetValue(ref PropertyKey key, out PropVariant value);
        [PreserveSig] int SetValue(ref PropertyKey key, ref PropVariant value);
        [PreserveSig] int Commit();
    }

    [DllImport("shell32.dll", CharSet = CharSet.Unicode)]
    static extern int SHGetPropertyStoreFromParsingName(string path, IntPtr bindCtx, int flags, ref Guid iid,
        [MarshalAs(UnmanagedType.Interface)] out IPropertyStore store);

    [DllImport("ole32.dll")]
    static extern int PropVariantClear(ref PropVariant value);

    // AppUserModelID is the shortcut's System.AppUserModel.ID read through
    // the shell's property store, null when it has none.
    public static string AppUserModelID(string path) {
        Guid iid = typeof(IPropertyStore).GUID;
        IPropertyStore store;
        Marshal.ThrowExceptionForHR(SHGetPropertyStoreFromParsingName(path, IntPtr.Zero, 0, ref iid, out store));
        try {
            PropertyKey key = new PropertyKey { fmtid = new Guid("9F4C2855-9F79-4B39-A8D0-E1D42DE1D5F3"), pid = 5 };
            PropVariant v;
            Marshal.ThrowExceptionForHR(store.GetValue(ref key, out v));
            try {
                if (v.vt == 0) return null;          // VT_EMPTY
                if (v.vt != 31) throw new InvalidOperationException("System.AppUserModel.ID has type " + v.vt);
                return Marshal.PtrToStringUni(v.p);  // VT_LPWSTR
            } finally { PropVariantClear(ref v); }
        } finally { Marshal.ReleaseComObject(store); }
    }
}
'@

function Why-Shortcut {
    if (-not (Test-Path -LiteralPath $Shortcut -PathType Leaf)) { return "no $Shortcut" }
    # LinkFlags (MS-SHLLINK 2.1.1), offset 20: HasDarwinID (0x1000) marks an
    # advertised shortcut, whose launch may run a repair.
    $bytes = [IO.File]::ReadAllBytes($Shortcut)
    if ([BitConverter]::ToUInt32($bytes, 20) -band 0x1000) { return 'the shortcut is advertised (HasDarwinID)' }
    $shell = New-Object -ComObject WScript.Shell
    $target = $shell.CreateShortcut($Shortcut).TargetPath
    if ($target -ine $UiExe) { return "the shortcut starts [$target], want [$UiExe]" }
    $id = [PcbSmokeShell]::AppUserModelID($Shortcut)
    if ($id -cne $AppID) { return "its System.AppUserModel.ID is [$id], want [$AppID]" }
    return ''
}

# --- the programs --------------------------------------------------------

# Run-Fresh LINE: cmd.exe /c LINE in a process whose PATH is the one a new
# terminal gets (the machine's then the user's, from the registry), not
# this script's, read before the install; in the scratch directory, since
# cmd.exe looks in the current directory before PATH.
function Run-Fresh([string] $line) {
    $psi = New-Object System.Diagnostics.ProcessStartInfo 'cmd.exe', "/d /c $line"
    $psi.UseShellExecute = $false
    $psi.WorkingDirectory = $Work
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.EnvironmentVariables['PATH'] = [Environment]::GetEnvironmentVariable('Path', 'Machine') + ';' + [Environment]::GetEnvironmentVariable('Path', 'User')
    $p = [System.Diagnostics.Process]::Start($psi)
    $out = $p.StandardOutput.ReadToEnd()
    $err = $p.StandardError.ReadToEnd()
    $p.WaitForExit()
    return [pscustomobject]@{ Code = $p.ExitCode; Out = $out; Err = $err }
}

function Why-PcbOnPath {
    $r = Run-Fresh 'where pcb.exe'
    $first = $r.Out -split "`r?`n" | Where-Object { $_ } | Select-Object -First 1
    if ($r.Code -ne 0 -or $first -ine $CliExe) { return "a new terminal finds [$first] (exit $($r.Code)), want [$CliExe]" }
    $r = Run-Fresh 'pcb version'
    $want = if ($Version) { '^pcb v' + [regex]::Escape($Version) + '[- ]' } else { '^pcb \S+ \(commit ' }
    if ($r.Code -ne 0 -or $r.Out -notmatch $want) { return "pcb version: exit $($r.Code), [$($r.Out.Trim())] [$($r.Err.Trim())], want $want" }
    return ''
}

function Why-PcbGone {
    $r = Run-Fresh 'where pcb.exe'
    $hit = @($r.Out -split "`r?`n" | Where-Object { $_ -and ($_ -ieq $CliExe) })
    if ($hit.Count) { return "a new terminal still finds $CliExe" }
    return ''
}

function Why-UiVersion {
    $out = Join-Path $Work 'ui-version.txt'
    $p = Start-Process -FilePath $UiExe -ArgumentList '--version' -RedirectStandardOutput $out -Wait -PassThru -NoNewWindow
    $text = (Get-Content -LiteralPath $out -Raw)
    if ($null -eq $text) { $text = '' }
    $want = if ($Version) { '^pcb-ui v' + [regex]::Escape($Version) + '[- ]' } else { '^pcb-ui \S+ \(commit ' }
    if ($p.ExitCode -ne 0 -or $text -notmatch $want) { return "pcb-ui.exe --version: exit $($p.ExitCode), [$($text.Trim())], want $want" }
    return ''
}

function Why-Uninstalled {
    if (Test-Path -LiteralPath $InstallDir) { return "$InstallDir is still there: $((Get-ChildItem -LiteralPath $InstallDir -Force | ForEach-Object Name) -join ', ')" }
    if (Test-Path -LiteralPath $Shortcut) { return "$Shortcut is still there" }
    if ([Microsoft.Win32.Registry]::CurrentUser.OpenSubKey($OwnKey)) { return "HKCU\$OwnKey is still there" }
    return ''
}

function Why-Markers {
    foreach ($d in $Markers) {
        if (-not (Test-Path -LiteralPath (Join-Path $d 'msi-smoke.marker'))) { return "$d\msi-smoke.marker is gone" }
    }
    return ''
}

# --- the run -------------------------------------------------------------

$saved = $null
$madeDirs = @()
$upgradeCode = $null
$exit = 1
try {
    Check 'the MSI exists' {
        if (-not (Test-Path -LiteralPath $Msi -PathType Leaf)) { return "no file $Msi" }
        if ($UpgradeMsi -and -not (Test-Path -LiteralPath $UpgradeMsi -PathType Leaf)) { return "no file $UpgradeMsi" }
        ''
    }
    $Msi = (Resolve-Path -LiteralPath $Msi).Path
    $productCode = Get-MsiProperty $Msi 'ProductCode'
    $upgradeCode = Get-MsiProperty $Msi 'UpgradeCode'
    Check 'pcb is not installed yet' {
        if (-not $upgradeCode) { return "$Msi has no UpgradeCode" }
        $p = Get-RelatedProducts $upgradeCode
        if ($p.Count) { return "products of UpgradeCode $upgradeCode are installed: $($p -join ', '); uninstall them first" }
        if (Test-Path -LiteralPath $InstallDir) { return "$InstallDir exists" }
        if (Test-Path -LiteralPath $Shortcut) { return "$Shortcut exists" }
        ''
    }
    if ($Version) {
        Check "ProductVersion is $Version" {
            $v = Get-MsiProperty $Msi 'ProductVersion'
            if ($v -ne $Version) { return "ProductVersion is [$v]" }
            ''
        }
    }
    if ($UpgradeMsi) {
        $UpgradeMsi = (Resolve-Path -LiteralPath $UpgradeMsi).Path
        $upgradeProduct = Get-MsiProperty $UpgradeMsi 'ProductCode'
        Check 'the upgrade MSI is another build of the same product' {
            if ((Get-MsiProperty $UpgradeMsi 'UpgradeCode') -ne $upgradeCode) { return 'another UpgradeCode' }
            if ($upgradeProduct -eq $productCode) { return "the same ProductCode $productCode" }
            ''
        }
    }

    New-Item -ItemType Directory -Path $Work | Out-Null
    $saved = [pscustomobject]@{ Path = Get-UserPath; Run = Get-Run }
    foreach ($d in $Markers) {
        if (-not (Test-Path -LiteralPath $d)) { New-Item -ItemType Directory -Path $d | Out-Null; $madeDirs += $d }
        Set-Content -LiteralPath (Join-Path $d 'msi-smoke.marker') -Value 'msi-smoke'
    }
    $base = if ($saved.Path) { Split-Path-Entries $saved.Path.Data } else { @() }
    $base = @($base | Where-Object { -not (Same-Dir $_ $InstallDir) })
    $others = $base + $OtherEntries
    Set-UserPath $others
    Set-Run $null

    # 1. The default install: both features, Start at login.
    Msiexec 'install (defaults)' @('/i', "`"$Msi`"")
    Check 'files present' { Why-Files @('pcb-ui.exe', 'pcb.exe') }
    Check 'shortcut: non-advertised, starts pcb-ui.exe, System.AppUserModel.ID' { Why-Shortcut }
    Check 'Run value: the data pcb-ui writes' { Why-Run $RunData }
    Check 'user PATH: the install dir once, the other entries intact' { Why-PathEntry 1 $others }
    Check 'a new terminal finds pcb.exe, pcb version runs' { Why-PcbOnPath }
    Check 'pcb-ui.exe --version' { Why-UiVersion }

    # 2. Repairs do not turn Start at login back on (Review Focus 1), nor off.
    Set-Run $null
    Msiexec 'repair /fa (Start at login turned off in pcb-ui)' @('/fa', "`"$Msi`"")
    Check 'repair /fa: no Run value' { Why-Run $null }
    Msiexec 'repair /fomus, the default repair' @('/fomus', "`"$Msi`"")
    Check 'repair /fomus: no Run value' { Why-Run $null }
    Check 'repair: files and shortcut still there' { $w = Why-Files @('pcb-ui.exe', 'pcb.exe'); if ($w) { $w } else { Why-Shortcut } }
    Set-Run $RunData
    Msiexec 'repair /famus (Start at login turned on in pcb-ui)' @('/famus', "`"$Msi`"")
    Check 'repair /famus: the Run value kept' { Why-Run $RunData }

    # 3. Cli removed, then added back (Review Focus 3).
    Msiexec 'REMOVE=Cli' @('/i', "`"$Msi`"", 'REMOVE=Cli')
    Check 'REMOVE=Cli: no pcb.exe' { Why-Files @('pcb-ui.exe') }
    Check 'REMOVE=Cli: no PATH entry, the other entries intact' { Why-PathEntry 0 $others }
    Check 'REMOVE=Cli: a new terminal no longer finds pcb.exe' { Why-PcbGone }
    Check 'REMOVE=Cli: the Run value kept' { Why-Run $RunData }
    Msiexec 'ADDLOCAL=Cli' @('/i', "`"$Msi`"", 'ADDLOCAL=Cli')
    Check 'ADDLOCAL=Cli: pcb.exe back' { Why-Files @('pcb-ui.exe', 'pcb.exe') }
    Check 'ADDLOCAL=Cli: the PATH entry back once' { Why-PathEntry 1 $others }
    Check 'ADDLOCAL=Cli: a new terminal finds pcb.exe' { Why-PcbOnPath }

    # 4. Uninstall: all gone but the data directories.
    Msiexec 'uninstall' @('/x', "`"$Msi`"")
    Check 'uninstall: program dir, shortcut and own registry key gone' { Why-Uninstalled }
    Check 'uninstall: no PATH entry, the other entries intact' { Why-PathEntry 0 $others }
    Check 'uninstall: the Run value gone' { Why-Run $null }
    Check 'uninstall: the data directories kept' { Why-Markers }

    # 5. Start at login unticked, a PATH that already holds the install dir
    # as a hand-made entry is written (no trailing backslash; Review Focus
    # 4), a Run value of another pcb-ui.exe (a zip install).
    $withDir = $others + @($InstallDir)
    Set-UserPath $withDir
    Set-Run $Elsewhere
    Msiexec 'install STARTATLOGIN=0 (PATH holding the dir, Run value of another pcb-ui.exe)' @('/i', "`"$Msi`"", 'STARTATLOGIN=0')
    Check 'STARTATLOGIN=0: the other Run value untouched' { Why-Run $Elsewhere }
    Check 'STARTATLOGIN=0: cleared, not 0 (the checkbox would show it ticked)' { Why-PropertyUnset 'STARTATLOGIN' }
    Check 'PATH holding the dir: still once' { Why-PathEntry 1 $others }
    Msiexec 'uninstall (Run value of another pcb-ui.exe)' @('/x', "`"$Msi`"")
    Check 'uninstall: the other Run value untouched' { Why-Run $Elsewhere }
    Check 'uninstall: the other PATH entries intact' { Why-PathEntry 0 $others }
    Check 'uninstall: program dir, shortcut and own registry key gone' { Why-Uninstalled }
    # A zip install in the same folder left Start at login on: unticked,
    # the install turns it off.
    Set-Run $RunData
    Set-UserPath $others
    Msiexec 'install STARTATLOGIN=0 (Run value of this pcb-ui.exe)' @('/i', "`"$Msi`"", 'STARTATLOGIN=0')
    Check 'STARTATLOGIN=0: no Run value' { Why-Run $null }
    Msiexec 'uninstall' @('/x', "`"$Msi`"")
    Check 'uninstall: program dir, shortcut and own registry key gone' { Why-Uninstalled }

    $elsewhereDir = Join-Path $Work 'elsewhere'
    MsiexecRefused 'install INSTALLFOLDER=<another folder> refused' @('/i', "`"$Msi`"", "INSTALLFOLDER=`"$elsewhereDir\`"")
    Check 'INSTALLFOLDER refused: nothing written there, no Run value, no PATH entry' {
        if (Test-Path -LiteralPath $elsewhereDir) { return "$elsewhereDir exists" }
        $w = Why-Run $null; if ($w) { return $w }
        Why-PathEntry 0 $others
    }

    # A default install over a zip install elsewhere (Review Focus 5): the
    # Run value then starts the MSI's pcb-ui.exe, and the uninstall,
    # finding it this install's, removes it.
    Set-Run $Elsewhere
    Msiexec 'install (defaults) over a Run value of another pcb-ui.exe' @('/i', "`"$Msi`"")
    Check 'install over a zip install: the Run value starts the MSI''s pcb-ui.exe' { Why-Run $RunData }
    Msiexec 'uninstall' @('/x', "`"$Msi`"")
    Check 'uninstall: the Run value, now this install''s, gone' { Why-Run $null }
    Check 'uninstall: program dir, shortcut and own registry key gone' { Why-Uninstalled }

    # 6. Major upgrades keep Start at login as pcb-ui left it.
    if ($UpgradeMsi) {
        Msiexec 'install (defaults)' @('/i', "`"$Msi`"")
        Set-Run $null
        Msiexec 'upgrade (Start at login turned off in pcb-ui)' @('/i', "`"$UpgradeMsi`"")
        Check 'upgrade: the new product replaced the old one' {
            if (Test-Installed $productCode) { return "$productCode is still installed" }
            if (-not (Test-Installed $upgradeProduct)) { return "$upgradeProduct is not installed" }
            ''
        }
        Check 'upgrade: files present' { Why-Files @('pcb-ui.exe', 'pcb.exe') }
        Check 'upgrade: no Run value' { Why-Run $null }
        Check 'upgrade: the PATH entry once' { Why-PathEntry 1 $others }
        Set-Run $RunData
        Msiexec 'upgrade (Start at login turned on in pcb-ui)' @('/i', "`"$Msi`"")
        Check 'upgrade: the Run value kept' { Why-Run $RunData }
        Check 'upgrade: the shortcut' { Why-Shortcut }
        Msiexec 'uninstall' @('/x', "`"$Msi`"")
        Check 'uninstall after upgrades: all gone, data kept' {
            $w = Why-Uninstalled; if ($w) { return $w }
            $w = Why-Run $null; if ($w) { return $w }
            $w = Why-PathEntry 0 $others; if ($w) { return $w }
            Why-Markers
        }
    }
    $exit = 0
} catch [System.OperationCanceledException] {
    $exit = 1
} catch {
    Write-Output "FAIL $($script:check): $($_.Exception.Message) ($($_.InvocationInfo.PositionMessage))"
    $exit = 1
} finally {
    # Leave the machine as it was: uninstall what is left, then put the
    # PATH and the Run value back, then remove the markers.
    if ($upgradeCode -and $saved) {
        foreach ($p in (Get-RelatedProducts $upgradeCode)) {
            Start-Process -FilePath msiexec.exe -ArgumentList @('/x', $p, '/qn', '/norestart') -Wait | Out-Null
        }
    }
    if ($saved) {
        Set-Value 'Environment' 'Path' $saved.Path
        Set-Value $RunKey 'pcb-ui' $saved.Run
        foreach ($d in $Markers) { Remove-Item -LiteralPath (Join-Path $d 'msi-smoke.marker') -ErrorAction SilentlyContinue }
        foreach ($d in $madeDirs) { Remove-Item -LiteralPath $d -ErrorAction SilentlyContinue }
    }
    if ($exit -eq 0) { Remove-Item -LiteralPath $Work -Recurse -ErrorAction SilentlyContinue }
}
if ($exit -eq 0) { Write-Output 'PASS' } elseif (Test-Path -LiteralPath $Work) { Write-Output "logs: $Work" }
exit $exit
