# holepunch

A CLI tool that manages AWS SSM port-forwarding tunnels with local DNS names, auto-reconnect, and per-service loopback IPs. Connect to your RDS, ElastiCache, OpenSearch, and Elasticsearch instances using real hostnames and ports — as if they were running locally.

```
$ holepunch up
Started profiles: [staging prod]

$ holepunch status
PROFILE  SERVICE     DNS                    ADDRESS           STATE        UPTIME
staging  opensearch  opensearch.staging     127.0.0.119:9443  ● Connected  2m31s
staging  postgres    postgres.staging       127.0.0.179:5432  ● Connected  2m31s
staging  redis       redis.staging          127.0.0.69:6379   ● Connected  2m31s
prod     postgres    postgres.prod          127.0.0.252:5432  ● Connected  2m31s
prod     redis       redis.prod             127.0.0.106:6379  ● Connected  2m31s

$ psql -h postgres.staging -U myuser
$ redis-cli -h redis.staging
$ curl http://opensearch.staging:9443/_cat/indices
```

## Features

- **DNS names instead of localhost** — each service gets a unique `127.0.0.x` loopback IP and a DNS name via an embedded DNS resolver. No `/etc/hosts` manipulation.
- **Real ports** — connect to `postgres.staging:5432`, not `localhost:29432`. Each service has its own IP, so ports don't conflict.
- **Auto-reconnect** — SSM sessions drop. Holepunch detects it and reconnects with exponential backoff (1s → 30s). You don't notice.
- **Multiple profiles** — run staging and prod tunnels simultaneously. Each profile is independent.
- **Sigv4 signing proxy** — OpenSearch with IAM auth just works. The built-in signing proxy handles AWS request signing transparently, including OpenSearch Dashboards in the browser.
- **Background daemon** — `holepunch up` launches a daemon and returns. Tunnels stay alive in the background. CLI communicates via unix socket.
- **macOS native** — uses `/etc/resolver/` for DNS routing and `lo0` aliases for loopback IPs. One-time `sudo` setup, then everything runs unprivileged.

## Prerequisites

- macOS (Linux support planned)
- [AWS CLI v2](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html)
- [Session Manager Plugin](https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html)
- Go 1.23+ (for building from source)
- SSM-managed EC2 instance in each VPC (bastion/jump host)

## Install

```bash
go install github.com/Kwutzke/holepunch@latest
```

Or build from source:

```bash
git clone https://github.com/Kwutzke/holepunch.git
cd holepunch
go build -o holepunch .
```

## Quick start

### 1. Create a config

```bash
mkdir -p ~/.holepunch
```

```yaml
# ~/.holepunch/config.yaml
profiles:
  staging:
    aws_profile: dev          # AWS CLI profile name
    aws_region: eu-central-1
    target: i-0abc123def456   # SSM-managed EC2 instance in the VPC
    services:
      - name: postgres
        dns_name: postgres.staging
        remote_host: mydb.cluster-xyz.eu-central-1.rds.amazonaws.com
        remote_port: 5432

      - name: redis
        dns_name: redis.staging
        remote_host: my-redis.abc123.euc1.cache.amazonaws.com
        remote_port: 6379

      - name: opensearch
        dns_name: opensearch.staging
        remote_host: vpc-my-os-domain.eu-central-1.es.amazonaws.com
        remote_port: 443
        local_port: 9443          # ports < 1024 need a high alternative
        sigv4_service: es         # enables automatic AWS request signing

  prod:
    aws_profile: prod
    aws_region: eu-central-1
    target: i-0def789ghi012
    services:
      - name: postgres
        dns_name: postgres.prod
        remote_host: prod-db.cluster-xyz.eu-central-1.rds.amazonaws.com
        remote_port: 5432
```

### 2. Run setup (one time, needs sudo)

```bash
sudo holepunch setup
```

This creates:
- `/etc/resolver/<tld>` files — routes DNS queries to holepunch's embedded resolver
- Loopback aliases on `lo0` — gives each service its own `127.0.0.x` IP
- A LaunchDaemon — so loopback aliases survive reboots

### 3. Connect

```bash
holepunch up              # start all profiles
holepunch up staging      # start one profile
holepunch status          # check connections
holepunch down            # stop all + kill daemon
```

## Commands

| Command | Description |
|---|---|
| `holepunch up [profile...]` | Start tunnels. No args = all profiles. Launches daemon automatically. |
| `holepunch down [profile...]` | Stop profiles. No args = stop all + kill daemon. |
| `holepunch down -f` | Force kill an unresponsive daemon. |
| `holepunch restart` | Stop everything, restart daemon, reconnect all profiles. |
| `holepunch status` | Show connection states, addresses, uptime, errors. |
| `holepunch logs -f` | Stream daemon events (connections, reconnects, errors). |
| `holepunch setup` | One-time setup: DNS resolver files + loopback aliases (needs sudo). |
| `holepunch unsetup` | Remove all setup artifacts. |

## Config reference

```yaml
profiles:
  <profile-name>:
    aws_profile: <string>       # AWS CLI profile (supports SSO)
    aws_region: <string>        # AWS region
    target: <string>            # SSM-managed EC2 instance ID (bastion)
    services:
      - name: <string>          # service identifier
        dns_name: <string>      # local DNS name (e.g. "postgres.staging")
        remote_host: <string>   # AWS endpoint hostname or private IP
        remote_port: <int>      # remote service port
        local_port: <int>       # local port (defaults to remote_port)
        sigv4_service: <string> # optional: AWS service name for request signing (e.g. "es")
```

### Notes

- **`local_port`** — defaults to `remote_port`. Use a high port (>1024) for privileged services like HTTPS (443) since the daemon runs unprivileged.
- **`sigv4_service`** — when set, holepunch runs an HTTP reverse proxy that signs every request with your AWS credentials (resolved from `aws_profile`). Required for IAM-authenticated OpenSearch. Set to `es` for OpenSearch/Elasticsearch Service.
- **`dns_name`** — fully configurable. The TLD determines which `/etc/resolver/` file is created during setup. Multiple TLDs are supported.
- **`target`** — the SSM-managed EC2 instance used as the tunnel endpoint. Must have network access to the `remote_host`. Typically a bastion or ECS instance in the same VPC.

## How it works

```
┌─────────┐     ┌──────────────────────────────────────────┐
│  psql   │────▶│  127.0.0.179:5432  (TCP proxy)           │
│         │     │       │                                   │
└─────────┘     │       ▼                                   │
                │  127.0.0.1:<random> (SSM tunnel)          │
┌─────────┐     │       │                                   │
│ browser │────▶│  127.0.0.119:9443  (sigv4 signing proxy)  │
│         │     │       │                                   │   holepunch
└─────────┘     │       ▼                                   │   daemon
                │  127.0.0.1:<random> (SSM tunnel)          │
                │       │                                   │
                │  DNS: opensearch.staging → 127.0.0.119    │
                │       postgres.staging  → 127.0.0.179     │
                │       (embedded DNS on 127.0.0.1:15353)   │
                └───────┼───────────────────────────────────┘
                        │
                        ▼  AWS SSM (encrypted)
                ┌───────────────┐
                │  EC2 bastion  │──▶ RDS / ElastiCache / OpenSearch
                └───────────────┘
```

1. **DNS** — macOS `/etc/resolver/` routes queries for configured TLDs to holepunch's embedded DNS server (port 15353). Each service resolves to a unique `127.0.0.x` address.

2. **Loopback aliases** — `ifconfig lo0 alias 127.0.0.x` creates the addresses. A LaunchDaemon recreates them on reboot.

3. **SSM tunnel** — `aws ssm start-session` opens a port-forwarding session through the bastion to the remote service. Binds to `127.0.0.1:<random-high-port>`.

4. **TCP proxy** — listens on `127.0.0.x:<real-port>` and forwards to the SSM tunnel on `127.0.0.1:<random-port>`. This bridges the unique IP to the SSM-bound port.

5. **Sigv4 proxy** (optional) — for services with IAM auth (OpenSearch), an HTTP reverse proxy signs every request with AWS credentials before forwarding. Works with browsers, curl, and any HTTP client.

6. **Auto-reconnect** — each service runs an independent state machine. When an SSM session drops, holepunch transitions to `Reconnecting`, applies exponential backoff, and retries until the session is restored.

## Architecture

The codebase is designed for extensibility (TUI planned):

```
cmd/              CLI layer (cobra) — thin, delegates to daemon
internal/
  config/         YAML config parsing + validation
  state/          Service state machine with transition validation
  reconnect/      Exponential backoff calculator
  ip/             Deterministic loopback IP allocator (FNV hash)
  dns/            /etc/hosts manager (legacy, not used in default mode)
  resolver/       Embedded DNS server + engine adapter
  session/        SSM session lifecycle (start, wait, stop)
  proxy/          TCP proxy + sigv4 signing reverse proxy
  engine/         Orchestrator: state machines, events, reconnect loops
  daemon/         Unix socket server/client, PID lock, lifecycle
```

The engine emits events on a Go channel. The CLI prints them as log lines. A future TUI subscribes to the same channel for a live dashboard — no engine changes needed.

## License

MIT
