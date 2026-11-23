# Deploy

How the unit and the environment template in this directory are laid out on a real
host. Paths and the account name are placeholders; substitute your own.

## The layout

The server runs beside the Matrix homeserver and the bridge, as an ordinary user,
with everything it needs in one directory:

```
/srv/matrix/
├── README.md                the operator's map of the whole stack
├── synapse -> /etc/matrix-synapse
├── synapse-data -> /var/lib/matrix-synapse
├── mautrix-whatsapp -> /opt/mautrix-whatsapp
└── matrix-whatsapp-mcp/
    ├── bin/matrix-whatsapp-mcp
    ├── etc/env              root:matrix-mcp 0640, the only secret
    ├── state/allow.txt      the send allowlist, crypto store beside it
    └── logs/                only used when output is redirected off journald
```

The two symlinks point at the homeserver and bridge installations, which stay where
their own upgrades expect them. `/srv/matrix/README.md` is yours to write: one page
covering how the pieces fit, where the backups go and who to call.

| Path | Owner | Mode | Notes |
| --- | --- | --- | --- |
| `bin/` | `root:root` | `0755` | Replaced on install, never written by the service. |
| `etc/env` | `root:matrix-mcp` | `0640` | **Secret.** Holds `MATRIX_ACCESS_TOKEN`; readable by the service user's group so `chats` and `doctor` work when run as that user. |
| `state/` | `matrix-mcp:matrix-mcp` | `0750` | Allowlist plus the crypto store when E2EE is on. Private, and the only state worth backing up. |
| `logs/` | `matrix-mcp:matrix-mcp` | `0750` | Unused under journald. |

## Install

Copy the bundle — the built binary, `matrix-whatsapp-mcp.service` and
`matrix-whatsapp-mcp.env.example` — onto the host, then from the directory you
copied it to:

```sh
sudo useradd --system --home /srv/matrix/matrix-whatsapp-mcp --shell /usr/sbin/nologin matrix-mcp

sudo install -d -o root -g root -m 0755 /srv/matrix/matrix-whatsapp-mcp/bin
sudo install -d -o root -g matrix-mcp -m 0750 /srv/matrix/matrix-whatsapp-mcp/etc
sudo install -d -o matrix-mcp -g matrix-mcp -m 0750 \
  /srv/matrix/matrix-whatsapp-mcp/state /srv/matrix/matrix-whatsapp-mcp/logs

sudo install -m 0755 matrix-whatsapp-mcp /srv/matrix/matrix-whatsapp-mcp/bin/
sudo install -m 0640 -o root -g matrix-mcp matrix-whatsapp-mcp.env.example \
  /srv/matrix/matrix-whatsapp-mcp/etc/env
sudo -e /srv/matrix/matrix-whatsapp-mcp/etc/env    # homeserver, user ID, access token

sudo install -m 0644 matrix-whatsapp-mcp.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now matrix-whatsapp-mcp
```

The account name in the unit is an example. If you create a different one, change
`User=`, `Group=` and the `env` group to match — the account that owns `state/` is
the one the service has to run as.

Set `MATRIX_ALLOWLIST_FILE` in the env file to
`/srv/matrix/matrix-whatsapp-mcp/state/allow.txt`. The unit sets `ProtectHome=true`,
so the built-in default under `$HOME` does not exist for the service and must not be
relied on.

## The three commands

Start it, and check it came up:

```sh
sudo systemctl enable --now matrix-whatsapp-mcp
systemctl status matrix-whatsapp-mcp
journalctl -u matrix-whatsapp-mcp -f
```

Check the configuration against the homeserver, as the service user with its
environment:

```sh
sudo -u matrix-mcp bash -c 'set -a; . /srv/matrix/matrix-whatsapp-mcp/etc/env; set +a; exec /srv/matrix/matrix-whatsapp-mcp/bin/matrix-whatsapp-mcp doctor'
```

Allow a chat to receive messages — invite the bot from Element, read the number from
`chats`, then allow it:

```sh
sudo -u matrix-mcp bash -c 'set -a; . /srv/matrix/matrix-whatsapp-mcp/etc/env; set +a; exec /srv/matrix/matrix-whatsapp-mcp/bin/matrix-whatsapp-mcp chats'
sudo -u matrix-mcp bash -c 'set -a; . /srv/matrix/matrix-whatsapp-mcp/etc/env; set +a; exec /srv/matrix/matrix-whatsapp-mcp/bin/matrix-whatsapp-mcp chats allow --index 2'
```

The running server re-reads the allowlist within a second. No restart, no dropped
connection. Restart only after editing `etc/env`.

The full picture — hardening flags, ownership, logs, troubleshooting — is in
[`docs/operations.md`](../docs/operations.md#running-it-beside-the-homeserver).
