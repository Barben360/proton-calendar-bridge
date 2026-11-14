# Run in a container

How to run pcb in Docker with Compose: the image, the `/data` volume, the key file, the first login, and what to do when the container stops or restarts in a loop. Getting the image: [install](install.md#the-container-image).

## What the image does

The image runs pcb serve as the entrypoint, so you never need `pcb install` or systemd inside it.

| | |
|---|---|
| Base | distroless, no shell |
| User | UID 65532 |
| Volume | one, `/data` (config, state, certificate) |
| Command | `pcb serve --wait-for-login --listen 0.0.0.0:1080` |
| Port | 1080, **self-signed TLS** |
| Health check | `pcb status --json --daemon` |

It comes up healthy with no login and waits for one. CalDAV answers 401 until a login creates the first client.

The image is `ghcr.io/barben360/proton-calendar-bridge`, a public package for amd64 and arm64: `docker pull` needs no login. Or `pcb:local` from `make image`.

## Tags

Images are pushed when a release is published, never from main.

| Tag | Moves to | Pushed for |
|---|---|---|
| `X.Y.Z` | never: a released tag is not rebuilt | every release |
| `X.Y` | the newest patch of that line | a stable release |
| `latest` | the newest stable release by version, not by date | a stable release |
| `X.Y.Z-rc.N` | never | a pre-release, which gets this tag only |
| `sha-<12 hex>` | never | a development build of one commit, started by hand |

A patch release of an older line (`1.2.4` after `1.3.0`) does not take `latest`. Pin `X.Y.Z` for a server you do not want to change under you.

### Check an image

Each image carries a GitHub attestation: proof that this repository's release workflow built it.

```sh
gh attestation verify oci://ghcr.io/barben360/proton-calendar-bridge:latest --repo Barben360/proton-calendar-bridge
```

## Prepare `/data`

`/data` must be writable by UID 65532, and its `config` and `state` directories must be 0700, else `pcb serve` exits 20.

- A **named volume** gets that from the image.
- A **bind mount** does not. Prepare it on the host:

```sh
mkdir -p data/config data/state && sudo chown -R 65532:65532 data && chmod 700 data/config data/state
```

## Compose file and config

`compose.yaml`, next to `config.toml` and `pcb_key`:

```yaml
services:
  pcb:
    image: ghcr.io/barben360/proton-calendar-bridge:latest   # or pcb:local from make image
    container_name: pcb
    restart: unless-stopped
    ports:
      - "1080:1080"                   # TLS; publish the same port: the printed URL carries the container's 1080
    volumes:
      - pcb-data:/data                # one volume: config, state, certificate
    configs:
      - source: pcb_config
        target: /data/config/config.toml
    secrets:
      - source: pcb_key
        target: pcb_key               # /run/secrets/pcb_key, with the host file's owner and mode (no uid/mode outside swarm)
volumes:
  pcb-data:
configs:
  pcb_config:
    file: ./config.toml
secrets:
  pcb_key:
    file: ./pcb_key                   # head -c 32 /dev/urandom > pcb_key  (exactly 32 bytes)
```

`config.toml`:

```toml
[caldav]
hostnames = ["pcb.lan"]               # names clients use: in the certificate, the first one in the printed URL
[secrets]
key_file = "/run/secrets/pcb_key"     # the key on another mount than the sealed data
```

Other `[caldav]` keys: [Thunderbird](thunderbird.md#the-caldav-listener).

## The key file

pcb seals your Proton session and the CalDAV passwords with a 32-byte key ([security](security.md#the-sealing-key)). Without `[secrets] key_file`, the key lives in `/data/config`, beside the data it protects; the compose secret above keeps it on another mount.

Outside swarm, compose bind-mounts a file secret with the host file's owner and mode, and ignores `uid`, `gid` and `mode`. pcb runs as 65532 and refuses a key file others can read (mode 0444 included). So always give the key to UID 65532 on the host before the first start:

```sh
head -c 32 /dev/urandom > pcb_key
sudo chown 65532:65532 pcb_key && sudo chmod 600 pcb_key   # unconditional: compose keeps the host owner and mode
```

A key pcb cannot read stops `pcb login` and `pcb client add` with "permission denied", and `pcb doctor` says so on its `key file` line (exit 10). A key of the wrong size gets exit 10 too, with its size.

## Start it and log in

```sh
docker compose up -d
docker exec -it pcb pcb login     # asks for the username, password and 2FA code; creates the client "default"
docker exec pcb pcb client show   # URL, username, password and the certificate fingerprint for Thunderbird
docker exec pcb pcb status        # live state
docker exec pcb pcb doctor        # every check, with the fix
```

`pcb client show` prints the URL with the first of `hostnames`, and the certificate's fingerprint. Thunderbird shows a certificate exception dialog once: confirm it only when its fingerprint equals the printed one. Then [connect Thunderbird](thunderbird.md#connect-thunderbird).

## Wrong passwords behind the userland proxy

Five wrong CalDAV passwords from one IP in a minute lock that IP for the rest of the minute. Docker's port publishing keeps the client's source IP, but with the userland proxy (rootless Docker, some setups) every client shares one IP, so one device's typos lock them all for that minute. A known limitation, tracked in the [open questions](../spec/OPEN.md).

## When the container stops or restarts in a loop

`docker logs pcb` ends with the error and its hint.

| Exit | Means | Examples |
|---|---|---|
| 10 | a fault you must repair | a lost or unreadable key, a damaged cache, an unreadable `state.json` or `layout.json`, an older pcb's files beside the per-account layout, a bad `config.toml` |
| 20 | a fault that may pass | the port in use, `/data` not writable by 65532 |

`restart: unless-stopped` restarts after either: Docker has no equivalent of the systemd units' `RestartPreventExitStatus=10`. And `docker exec` cannot reach a stopped container. So stop it, run the CLI in a one-off container on the same volume, secret and config, then start it again:

```sh
docker compose stop pcb
docker compose run --rm --no-deps --entrypoint pcb pcb doctor   # every check, with the fix; also: status, logout
docker compose up -d
```

What the log says, and the fix:

| The log says | Fix |
|---|---|
| `the sealing key file /run/secrets/pcb_key is missing`, or `the stored secrets cannot be opened with the key file ...` (a different key) | Restore `pcb_key` from a backup. Or discard the stored session with `docker compose run --rm --no-deps --entrypoint pcb pcb logout`, start the container, then `docker exec -it pcb pcb login`. |
| `permission denied` or `mode 0444` on the key file | The host file's owner or mode: `sudo chown 65532:65532 pcb_key && sudo chmod 600 pcb_key`. |
| anything else | Follow the hint in the log, running the CLI through `docker compose run` as above. |

After a new key, CalDAV clients sealed with the lost key cannot be shown again: `pcb client show` names `pcb client rotate NAME` for them, and `pcb doctor` warns once per such client (`caldav client NAME: password cannot be shown`, fix `pcb client rotate NAME`).

## How it was tested

- `make container-smoke` checks the image: healthy with no account, 401 over TLS with or without credentials, UID 65532, healthy after `docker kill` ([development](development.md#smoke-tests)).
- The compose file above, the compose secret and `config` mount, and a login in the container were checked live against a Proton test account on 2026-11-13; Thunderbird was not part of that check ([status](status.md)).
