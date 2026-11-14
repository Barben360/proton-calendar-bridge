# E15 — Secret sealing on headless Linux

Feeds C2, SPEC §7.3 (secret sealing order), `pcb doctor`.

## Question

What can seal the Proton session and key passphrase at rest on two kinds of machine (the desktop: an Arch Linux desktop with a TPM2 and a Secret Service, systemd 262; the server: a headless Debian 13 server without a desktop session, systemd 257, where the bridge runs in Docker), what does each backend actually protect against, and what can the container use?

## Method

Read-only checks on each machine, 2026-11-11. Round trips use the dummy value `pcb-e15-dummy` in a fresh `mktemp -d` directory deleted in the same command. No Proton call, no package installed, nothing on the server touched beyond that. The server was reached over SSH (no TTY, no desktop session). All commands ran from an interactive shell or SSH session, **not inside a systemd user service**; whether the same holds for a boot/linger-started service was not tested.

## Results

| Check | desktop | server |
|---|---|---|
| OS | Arch Linux based | Debian 13 (trixie) |
| systemd | 262 | 257 |
| `systemd-analyze has-tpm2` | `yes`, rc 0 | `partial`, rc 16: `-libtss2-esys.so.0`, `-libtss2-rc.so.0`, `-libtss2-mu.so.0` missing |
| `/dev/tpmrm0` | present | present |
| `org.freedesktop.secrets` on the user bus | 1 owner, in an interactive session only | 0; a user bus exists over SSH but only systemd and busctl are on it |
| `systemd-creds encrypt/decrypt --user` | round trip ok | round trip ok |
| `--user --with-key=tpm2` | refused: `Selected key not available in --uid= scoped mode, refusing.` | same |
| system scope round trip (root) | **untested** (needs root; not run, ruling R18) | ok; warning `credential.secret ... not located on encrypted media, using anyway`; the host key is readable by root only |
| system scope with TPM2 | **untested** | **untested** (libtss2 not installed; installing is out of scope) |

Docker on the server: 29.9.3, Compose v5.7.0.

Sources and caveats:
- `systemd-creds(1)` (the desktop, systemd 262): `--user` (added in version 256) incorporates the user's UID, username and the machine-id into the key, and the credential can be decrypted only from that user's context unless privileges can be acquired. The man page's host-key mode: key in `/var/lib/systemd/credential.secret`, "only accessible to the root user"; TPM2 mode: key "not stored on the host system". `--user` therefore needs systemd >= 256: both machines qualify (262, 257), and both round trips passed.
- `systemd-creds has-tpm2` was deprecated in favour of `systemd-analyze has-tpm2`; observed on 262 (the desktop), and the server (257) printed the same redirect message. The exact version that introduced it is not established here.
- TPM2 sealing was never exercised on either machine. Every TPM2 statement below is from the man page, not measured.
- Secret Service: the bus name was owned on the desktop in an interactive session. It needs a session with an unlocked keyring (graphical login). A user service started at boot or by linger may find the keyring locked or absent. "Present" on the desktop means interactive session only.

### Protection per backend

Y = protects, N = does not. Statements marked (man) come from systemd-creds(1) and were not tested.

| Backend | Offline disk theft, no FDE | Offline theft, with FDE | Another local user | Same-user process / malware | Local root | Backup/snapshot of the data volume only | Casual disclosure (logs, `cat` of data) |
|---|---|---|---|---|---|---|---|
| Secret Service (unlocked login keyring) | partial (untested; depends on login-password strength: reasoned, the keyring file is encrypted under a key derived from the login password) | Y (reasoned) | Y (reasoned) | N (any process of the session can ask the unlocked keyring) | N (can read the session's memory/bus) | Y (reasoned) | Y (reasoned) |
| systemd-creds user scope, host key | N (key file on same disk; attacker with the disk has both) | Y (reasoned: FDE covers both) | Y (man: UID bound, host key root-only) | N (user can decrypt its own credentials) | N (root has the host key) | Y if the backup excludes `/var/lib/systemd` | Y |
| systemd-creds system scope, host key | N | Y | Y | Y (man) only if the service runs as a different user than the attacker; N for the service's own user | N | Y if the backup excludes `/var/lib/systemd` (reasoned) | Y |
| systemd-creds system scope, TPM2 | Y (man, untested) | Y (man, untested) | Y (man, untested) | N for the service's own user | N (root can ask the TPM to unseal) | Y | Y |
| Key file 0600 | N | Y | Y | N | N | Y only if the key lives on a different volume than the data | Y |
| Compose secret file (`/run/secrets/x`, from a host file) | N (host file) | Y | Y | N for any process with the same mount | N | Y (key is not in the data volume) | Y |

Plain reading: host-key credentials are no stronger than a 0600 key file against root or a same-user process. They help against casual disclosure and data-only backups, which a key file on a separate path also does. Only TPM2 binding, or a keyring encrypted under a strong login password (both untested here), helps against offline theft without FDE.

Restoring an older `secrets.sealed` is detected since M5 (a sealed generation mirrored in `state.json`, SPEC §7.3); restoring the whole state directory is not.

### Container (reasoned, no container run)

- A container has its own PID/mount namespaces and, by default, no `/run/systemd`, no host `/run/user/<uid>/bus`, no `/dev/tpmrm0`; a slim image has no systemd-creds and no PID 1 systemd. Reaching host systemd-creds or the session bus needs socket/device mounts that defeat isolation: not supported.
- Compose file-based `secrets:` bind-mounts a host file read-only at `/run/secrets/<name>` without Swarm. It is a delivery channel, not a sealing mechanism: strength equals the host file's permissions.
- A key on a volume separate from the data protects against a backup or snapshot of the data volume alone. It does not protect against host root, any container that has the same mounts, or a bind mount from the same host path (the key sits next to the data on the same host disk).

### What the M5 smoke tests show (2026-11-12)

Run locally with Docker; neither logs in, so no sealed Proton session was written.
- **Container** (`make container-smoke`, steps 1 to 9, PASS on commit e5faeb8): the distroless static image runs as UID 65532 with one volume `/data` (config and state directories `0700` 65532, inherited by a fresh named volume), comes up healthy with no login, answers 401 and 503 over its self-signed TLS, has no `secrets.sealed` and comes back healthy after `docker kill`. `pcb client add` works in it. With the default `key_file` (`/data/config/secrets.key`) the key sits on the data volume; the compose secret at `/run/secrets/pcb_key` of the README is the separation this file recommends, and it was **not** exercised by the smoke test; the live step 7.2.3 did: `status` showed `key_file: /run/secrets/pcb_key` and `pcb doctor` printed no same-volume warning [verified: live 2026-11-13, spec/experiments/M5-live.md]. A bind mount of `/data` must be writable by 65532, and the directories `0700`: with `0755` directories `pcb serve` exits 20 (sabotage S3 of Task 8).
- **systemd** (`make systemd-smoke`, a Debian 13 container with systemd as PID 1, `--privileged --cgroupns=host`, PASS on commit e5faeb8): `pcb install --user` and `--system` start the unit, it stays active, answers 503 to an authenticated CalDAV request while logged out, `systemd-analyze verify` is silent on both units, a second install changes nothing, and `uninstall --delete-data` removes the unit, the directories and the `pcb` user. The service context did not change what the key file needs (a path and mode 0600).
- Still untested (step 7): a sealed session written and read back inside a systemd user or system service, and inside the container with the compose secret; TPM2, desktop system scope, and Secret Service or `systemd-creds` sealing, which M5 did not build (the key file is the only sealer, SPEC §7.3).

## Conclusion

Verified: user-scope systemd-creds works (systemd 262 and 257, interactive/SSH context), Secret Service is present on the desktop in an interactive session only, none on the server, system-scope systemd-creds works on the server. Untested: TPM2 sealing anywhere, desktop system scope, a sealed session inside any systemd service or container (M5's smoke tests run pcb in both without a login).

One order per environment, justified by the table (the differences between backends are offline theft without FDE, which only TPM2 or a keyring under a strong login password could change, and backups):

| Environment | Order | Why |
|---|---|---|
| Desktop user service | 1. Secret Service (needs the unlocked keyring of an interactive login); 2. user-scope systemd-creds; 3. key file 0600 | Secret Service keeps its key out of the disk in clear (derived from the login password), so it may help against offline theft without FDE, depending on the password's strength (untested). A boot/linger service finds it locked, so it falls to user-scope creds, which equal a key file but are bound to UID and machine. |
| Server system service | 1. system-scope systemd-creds (host key); 2. key file 0600 on a separate path, `doctor` warns | No Secret Service headless. Host-key creds equal a key file for root and same-user attackers, but integrate with `LoadCredentialEncrypted=` and keep the key out of the service's data tree. TPM2 is not in the order until tested. |
| Container (server deployment) | 1. compose secret file at `/run/secrets/<name>`; 2. key file 0600 on a volume separate from the data | No systemd, no Secret Service in the container. A key on another volume defends data-volume backups only. |

`pcb doctor` reports the backend chosen and warns on the key-file fallback and when the key and the data share a volume.

## Feeds

SPEC §7.3 (order and marker), `pcb doctor` secret-backend report, container/compose documentation.
