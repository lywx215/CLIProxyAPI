# Deploy the local working tree to a Linux VPS

## Production maintenance gate

Only one CLIProxyAPI server may be under maintenance at a time across all Zeabur
and VPS instances. Complete its channel stoppage, drain, deployment, health
checks, real channel-call tests, and verified routing restoration before starting
the next server. Resolve failed recovery first. Do not overlap these stages or
push a shared branch that would automatically deploy multiple instances.

This user's `gemini30.pro` entry point must remain running. CLIProxyAPI restarts,
updates, redeployments, rollback, and container recreation must first disable all
related New API channels and verify that at least 95% of the baseline live work
has completed naturally. Preserve original channel states and restore only
previously enabled channels after health and management/model-list checks pass
and real model calls through those New API channels succeed while disabled.
Use each channel's configured mapping and request settings, including streaming
when applicable. A model list or HTTP 200 alone is insufficient. Restore only
previously enabled channels whose functional tests passed; leave failures off.
For a baseline N, no more than floor(0.05*N) baseline requests may remain.

When the completion count cannot be measured reliably, the user authorizes an
alternative after confirmed channel stoppage: observe 60 continuous seconds with
no new New API request logs, or wait 180 seconds from confirmed stoppage,
whichever occurs first. Use global New API request logs unless the user specifies
another scope. Failed log observations do not establish silence. This fallback
must be reported as log/time admission, not verified 95% completion.

The decision logic is shared with the [fleet update controller](../ops/README_CN.md).
A nonempty `maintenance` policy blocks `deploy`, `update`, and `restart` in this
standalone entry point. Use the fleet controller for production updates. `probe`,
`status`, `logs`, and offline `package` remain available here.
Do not remove the policy or use raw Docker/SSH/Zeabur commands to bypass the
controlled workflow.

Idle TCP keep-alive counts, historical usage, and Gemini-only budget counters do
not verify 95% completion. A current human request is still required for
production maintenance; a saved policy is not permission to change routing or
restart a service.

## Requirements

Run these commands from the local checkout. The server must already have Docker
and Docker Compose (v2 or later). The scripts do not install or upgrade the host's
Docker, Compose, BaoTa, or other applications. Python 3.10+ and Git are needed
locally; install the SSH dependency once:

```powershell
python -m pip install -r scripts/vps/requirements.txt
```

On Windows, `deploy-vps.cmd` also works when PowerShell script execution is
disabled. It accepts the Python command-line flags, for example:

```powershell
.\deploy-vps.cmd update --password
.\deploy-vps.cmd logs --password --lines 200
```

Double-clicking `deploy-vps.cmd` runs `deploy --password` using the saved local
profile. The PowerShell wrapper below requires a shell that permits local scripts.

## First deployment

```powershell
.\deploy-vps.ps1 probe -Server YOUR_SERVER -User root -PasswordAuth -AcceptNewHostKey
.\deploy-vps.ps1 deploy -Server YOUR_SERVER -User root -PasswordAuth
```

For a domain whose DNS already points at a server with BaoTa Nginx, add
`-Domain api.example.com` to `deploy`. The script creates a dedicated virtual host,
issues a Let's Encrypt certificate using the official Certbot container, and
installs a twice-daily certificate renewal cron job. It requires inbound ports
80/443, root privileges, and an active cron daemon. HTTPS setup uses Let's Encrypt's
terms of service and creates an ACME account without an email address. Existing
unmanaged virtual hosts are never overwritten. The virtual host is managed by
this script; it is not added to BaoTa's website database.

The first command saves a previously unknown SSH host key and prints its
fingerprint. Later connections check the saved key; changed host keys are rejected.
Passwords are prompted without echo and are never stored. For key authentication,
omit `-PasswordAuth` and optionally pass `-IdentityFile C:\path\to\id_ed25519`.
Use `-SshPort 2222` for a non-default SSH port. Non-root users need write access to
the deployment directory and Docker; `-UseSudo` uses non-interactive `sudo -n`.

Save non-secret defaults in the Git-ignored `.codex/vps-deploy.json` to avoid
repeating connection arguments. Password fields are intentionally unsupported:

```json
{
  "host": "YOUR_SERVER",
  "user": "root",
  "ssh_port": 22,
  "directory": "/opt/cliproxyapi",
  "project": "cliproxyapi-vps",
  "port": 8317,
  "domain": "api.example.com"
}
```

Each deployment packages the current on-disk tracked and non-ignored untracked
files, including uncommitted changes and `static/management.html`. It does not
require a commit or push. Runtime directories, `.env*`, private keys, and local
`config*.yaml` files other than `config.example.yaml` are excluded. Inspect any
additional untracked files before deploying. Symlinks and Git submodules are not
supported. A manifest records Git HEAD, dirty state, and a SHA-256 digest of the
actual archived file contents, including the panel.

The server builds a unique local Docker image and runs it in an isolated Compose
project. Builds finish before replacing a running container. Updates keep the
existing configuration, accounts, plugins, logs, data directory, and host port.
Failed startup or health checks trigger a rollback to the previous image; data and
configuration are shared across images, so this is not a data migration rollback.
Updates and restarts briefly interrupt active requests. Old images and source
releases are retained for inspection; there is no automatic Docker pruning.

## Update and daily management

With a saved profile:

```powershell
.\deploy-vps.ps1 update -PasswordAuth
.\deploy-vps.ps1 restart -PasswordAuth
.\deploy-vps.ps1 status -PasswordAuth
.\deploy-vps.ps1 logs -PasswordAuth -Lines 200
.\deploy-vps.ps1 logs -PasswordAuth -Follow
```

The portable Python entry point exposes the same commands:

```bash
python scripts/vps/deploy.py update --host YOUR_SERVER --password
python scripts/vps/deploy.py logs --host YOUR_SERVER --password --follow
```

Create a snapshot without contacting any server:

```powershell
.\deploy-vps.ps1 package
```

## Server files and BaoTa reverse proxy

The default deployment directory is `/opt/cliproxyapi`. A first deployment refuses
to adopt an existing nonempty directory. It creates:

- `shared/config.yaml`: server configuration, preserved on subsequent updates.
- `shared/initial-credentials.txt`: distinct generated management and client API
  keys, readable only by the deployment owner (mode 600). Read this file privately
  through SSH or the server's file manager; deployment output never prints keys.
- `shared/auths`, `shared/logs`, `shared/plugins`, `shared/data`: persistent files.
- `compose.yaml`, `current.env`, `current-manifest.json`: active deployment metadata.
- `releases/<release-id>`: source snapshots and previous-image metadata.

The API binds to `127.0.0.1:8317` on the server. Set `-Port 9321` on the initial
deployment to choose a different host port. Do not change the container's internal
`server.port` (8317). In BaoTa, configure a domain's reverse proxy target as
`http://127.0.0.1:8317`, enable HTTPS and WebSocket support, and disable proxy
buffering for streaming responses. The management page is `/management.html`;
the OpenAI-compatible API base path is `/v1`.

For direct public IP access, use `--public` with Python or `-PublicAccess` with
PowerShell on the first deployment. This publishes the host port on `0.0.0.0`;
the server and provider firewalls must permit the chosen port. Public access
still requires the client API key or management key for the respective APIs.
Updates preserve the original host binding as well as the port. The non-secret
profile can save `"public": true` for a dedicated new server. Do not use this
option to change the exposure of an existing production deployment.

When `-Domain` is supplied, the script configures this reverse proxy automatically
in `/www/server/panel/vhost/nginx/<domain>.conf`. Certificates are stored in
`tls/`, and the renewal hook is `renew-tls.sh` inside the deployment directory.
To manage an existing BaoTa site yourself, omit `domain` from the local profile.

Before a domain is configured, an SSH tunnel can access the service:

```bash
ssh -L 8317:127.0.0.1:8317 root@YOUR_SERVER
```

Open `http://127.0.0.1:8317/management.html` locally. Health checks use `/healthz`;
they do not make model calls. Import provider credentials through the management
page to start routing requests.

## Validation

```powershell
python -m unittest discover -s scripts/vps -p 'test_*.py' -v
```

The offline tests verify working-tree snapshot contents, secret exclusions,
panel hashing, preservation of credentials and host ports, failed-build safety,
and health-check rollback using a fake Docker executable. Bash is required for
the remote-control-flow tests (Git Bash works on Windows).
