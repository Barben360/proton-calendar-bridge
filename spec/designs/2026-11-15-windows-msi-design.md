# Windows MSI installer — design

- **Date**: 2026-11-15
- **Status**: approved by the author on 2026-11-15.
- **Decided by**: author (marked [author]); the rest is the agent's call, marked [call].
- **Amends**: decision 0018 ("Next: an MSI"), `spec/OPEN.md` (the MSI leaves "next projects"), SPEC §13 "Platforms", `docs/install.md`, `docs/desktop.md`, `docs/status.md`, the README, the release process (`spec/designs/2026-11-14-release-process-design.md` §5.1).

## 1. Intent

Windows users get the bridge from a zip today: unzip, run `pcb-ui.exe`, put `pcb.exe` on `PATH` by hand. The author wants an installer before the next release [author]: the MSI installs `pcb-ui` (required) and `pcb` (optional, on by default, on the user's `PATH`), a Start-menu shortcut that names the app to Windows (so toasts show pcb-ui's name), and "Start at login" [author]. The release waits for it [author].

## 2. Goals, non-goals, anti-goals

- Goals: `pcb_windows_amd64.msi` in each release next to the zip, attested and in `checksums.txt`; a per-user install with no administrator prompt; built and smoke-tested in CI on every pull request and in the release workflow before publishing; a short live step on the Windows VM.
- Non-goals: Authenticode signing (the MSI is unsigned, SmartScreen warns, as for the zip) [author]; a per-machine install; winget, Chocolatey or Scoop; arm64; a byte-identical MSI (WiX cannot produce one; the checksum and the attestation cover it) [call].
- Anti-goals: an installer that needs administrator rights; one that deletes the user's accounts, secrets or settings at uninstall; one that fights pcb-ui's own "Start at login" switch.

## 3. Approaches

| Approach | Trade-off | Verdict |
|---|---|---|
| WiX Toolset on the Windows runner | The standard MSI tool; feature-tree UI for the optional `pcb`; needs a Windows job | Chosen [call, approved] |
| `wixl` (msitools) on Linux | Builds on Linux, but no WiX UI dialogs (no feature selection) | Rejected: cannot make `pcb` deselectable |
| No MSI, a PowerShell install script | Simple, but no uninstall entry, no repair, no Start-menu ID for toasts | Rejected |

WiX version: **7.0.1**, with its OSMF EULA accepted in CI (`-acceptEula wix7`) [author]; extensions `WixToolset.UI.wixext` and `WixToolset.Util.wixext` at the same version.

## 4. Design

| Point | Decision |
|---|---|
| Scope | Per-user (`Package Scope="perUser"`), installed in `%LOCALAPPDATA%\Programs\pcb` [call, approved] |
| Features | `App` (`pcb-ui.exe`, required, `AllowAbsent="no"`), `Cli` (`pcb.exe` + the user's `PATH` entry, on by default, deselectable) [author] |
| Screens | WixUI_FeatureTree: Welcome, licence (the MIT `LICENSE` as RTF), features, install, finish with a "Launch pcb-ui" checkbox [call, approved] |
| Start at login | A ticked checkbox in the installer; when ticked, the MSI writes the same HKCU Run value pcb-ui writes (`pcb-ui` = `"<INSTALLFOLDER>pcb-ui.exe" --background`); pcb-ui's switch keeps working afterwards (the value is never a component KeyPath and repair must not recreate it after the user turned it off); uninstall removes it when it still starts this install's `pcb-ui.exe` [call, approved] |
| Shortcut | One non-advertised Start-menu shortcut to `pcb-ui.exe`, `System.AppUserModel.ID` = `io.github.barben360.pcb-ui` (pcb-ui's Fyne app ID, the one Fyne passes to the toast notifier) [author: shortcut; call: the ID] |
| PATH | User `PATH`, `Part="last"`, removed at uninstall or when `Cli` is deselected; new terminals see it (Windows Installer broadcasts the change) |
| Uninstall | Removes the program files, the shortcut, the `PATH` entry and the Run value; keeps `%APPDATA%\pcb` and `%LOCALAPPDATA%\pcb` (accounts, secrets, settings), as on Linux; wiping stays `pcb uninstall --delete-data` [call, approved] |
| Running pcb-ui | Closed before files are replaced or removed (Restart Manager, or `util:CloseApplication` if pcb-ui does not answer it: the builder checks on the VM which works with pcb-ui's session-end watcher) |
| Upgrades | `MajorUpgrade` with a fixed UpgradeCode, `AllowSameVersionUpgrades="yes"` (an MSI version has three fields, so `v0.2.0-rc.1` and `v0.2.0` are both 0.2.0 and the final must replace the candidate; the cost: installing the candidate over the final is also accepted); a downgrade message otherwise [call, approved] |
| Version | `ProductVersion` from the tag (`vX.Y.Z[-…]` → `X.Y.Z`); a non-tag build (pull requests) uses `0.0.0` or the last tag's numbers |

## 5. Testing

- Pull requests (`ci.yml`, Windows): build the MSI from the job's binaries; install silently (`msiexec /i … /qn`), check the files, the shortcut and its app ID, the Run value, the user `PATH` (a new process sees `pcb version`), `pcb-ui --version`; reinstall without `Cli` (`REMOVE=Cli` or `ADDLOCAL=App`): no `pcb.exe`, no `PATH` entry; uninstall: everything gone except the data directories. Each check fails when its part is missing.
- Release (`release.yml`): the MSI is built from the release's binaries, attested, listed in `checksums.txt`, and the Windows smoke job runs the same silent install, checks and uninstall on the real asset before publishing.
- Live (`spec/HUMAN-CHECKS.md` step 13, on the Windows VM as the standard user, no Proton login): install by double-click (screens, no administrator prompt, SmartScreen as expected), the Start-menu shortcut, a toast showing pcb-ui's name, Start at login across a restart, upgrade over the same version, uninstall keeping the data.

## 6. Left to the builder

The WiX source layout (`packaging/windows/`), the build script and its Makefile target, the RTF licence generation, the exact smoke script, how the release job passes binaries to the MSI job, ICE warnings (ICE91 is expected for a per-user package), and the docs wording.
