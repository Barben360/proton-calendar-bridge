# E15 — Secret sealing on headless Linux

Feeds C2, SPEC §7.3 (secret sealing order), `pcb doctor`.

## Question

What can seal the Proton session and key passphrase at rest on the author's two machines (blackwater: CachyOS desktop; morpheus: Debian 13 headless server running the bridge in Docker), and what can the container use?

## Method

Read-only checks on each machine, 2026-11-11. Round trips use the dummy value `pcb-e15-dummy`, written into a fresh `mktemp -d` directory deleted in the same command. No Proton call, no package installed, nothing on morpheus touched beyond that (no /etc, services, containers). morpheus was reached over SSH (no TTY, key auth), so it has no desktop login session.

Commands per machine: `systemctl --version`, `systemd-analyze has-tpm2` (what `systemd-creds has-tpm2` redirects to), `ls -l /dev/tpmrm0`, `busctl --user list | grep -c org.freedesktop.secrets`, `systemd-creds encrypt/decrypt --user`, and (morpheus) `sudo -n systemd-creds encrypt/decrypt` (system scope).

## Results

| Check | blackwater | morpheus |
|---|---|---|
| OS | CachyOS (Arch) | Debian 13 (trixie) |
| systemd | 262 (262.1-1-arch) | 257 (257.13-1~deb13u1) |
| `systemd-analyze has-tpm2` | `yes`, rc 0 (firmware, driver, system, subsystem, libraries all `+`) | `partial`, rc 16: firmware, driver, system, subsystem `+`; `-libraries` (`-libtss2-esys.so.0`, `-libtss2-rc.so.0`, `-libtss2-mu.so.0`) |
| `/dev/tpmrm0` | present, `crw-rw---- root tss` | present, `crw------- root root` (also `/dev/tpm0`) |
| Secret Service on session bus | `1` (a provider owns `org.freedesktop.secrets`) | `0`. Over SSH a user bus exists (`XDG_RUNTIME_DIR=/run/user/1000`, `user@1000.service` up, `Linger=no`) but only systemd and busctl are on it; no provider |
| `systemd-creds encrypt --user` + `decrypt --user` | encrypt rc 0, decrypt returns `pcb-e15-dummy`, rc 0 | encrypt rc 0, decrypt returns `pcb-e15-dummy`, rc 0 |
| `--user --with-key=tpm2` | refused: `Selected key not available in --uid= scoped mode, refusing.` | same refusal |
| system scope (root) round trip | not run (needs interactive sudo) | encrypt rc 0, decrypt returns `pcb-e15-dummy`, rc 0. Warning: `Credential secret file '/var/lib/systemd/credential.secret' is not located on encrypted media, using anyway.` Key file is `-r--------` root |

Docker on morpheus: Docker 29.9.3, Compose v5.7.0, security options apparmor, seccomp (builtin), cgroupns.

Notes:
- `systemd-creds has-tpm2` is deprecated and redirects to `systemd-analyze has-tpm2` (still prints the same table).
- User scope (systemd >= 256) works on both and is host-key based, not TPM-bound: the key is in the per-user scope, TPM2 is not selectable there. On morpheus the user-scope credential is decryptable by any process of that user on that host.
- TPM2 sealing on morpheus would need `libtss2` installed (not done, out of scope) and root (`/dev/tpmrm0` is root-only). Even then the system-scope host key at `/var/lib/systemd/credential.secret` sits unencrypted on disk unless TPM2 is used.

### Container (reasoned, no container run)

- A container has its own PID/mount namespaces and, by default, no `/run/systemd`, no host `/run/user/<uid>/bus`, no `/dev/tpmrm0`. `systemd-creds` is not in a typical slim image, and without PID 1 = systemd there is no credential store. Reaching the host's `systemd-creds` or session bus would mean mounting sockets/devices into the container, which defeats the isolation and is not a supported setup.
- Compose file-based `secrets:` (`secrets: x: {file: ./x}`) bind-mount the file read-only at `/run/secrets/x` in plain `docker compose` (no Swarm needed). Docker 29 / Compose 5 support it. This is a delivery channel for the data key (or an unseal passphrase), not a sealing mechanism: protection equals the host file's permissions.
- Linux has no default Secret Service in a container.

## Conclusion

systemd-creds is available on both machines (user scope since systemd >= 256: 262 and 257); TPM2 is usable only on blackwater; Secret Service only on blackwater; the container has none of them.

Sealing order per environment:

| Environment | Order |
|---|---|
| Desktop user service (blackwater) | 1. `systemd-creds` with TPM2 when `systemd-analyze has-tpm2` says `yes` and the scope allows it (system scope; user scope refuses `tpm2`); otherwise user-scope `systemd-creds` host key. 2. Secret Service (present). 3. key file 0600. Practical default for a user service: Secret Service (key tied to the login keyring), then user-scope `systemd-creds`. |
| Server system service (morpheus, no container) | 1. System-scope `systemd-creds` (works; TPM2 only if libtss2 present, else host key with doctor warning "credential.secret not on encrypted media / not TPM-bound"). 2. Secret Service not available headless. 3. key file 0600 on a separate path with a doctor warning. |
| Container (morpheus deployment) | No systemd-creds, no Secret Service. 1. Key file 0600 on a mounted volume separate from the data volume. 2. Optional compose `secrets:` file mount (`/run/secrets/<name>`) supplying the data key so the key is not in the data volume (`doctor` reports which source was used). The sealing strength is the host file permissions; document that plainly. |

Selection is per capability probe at start, not per OS name: try systemd-creds (`systemd-creds` binary and `has-tpm2`/round trip), then Secret Service (a name owner for `org.freedesktop.secrets`), then docker secret/key file; `pcb doctor` reports the chosen backend and warns on the key-file fallback.

## Feeds

SPEC §7.3 (marker updated to `[verified: E15]`), `pcb doctor` secret-backend report, container/compose documentation (key file + optional `secrets:`).
