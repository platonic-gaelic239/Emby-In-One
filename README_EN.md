# Emby-In-One

> **Version: V1.3**

[Changelog](Update.md) | [中文文档](README.md) | [Security Policy](SECURITY.md) | [Update Plan](Update%20Plan.md) | [V1.2.1 Documentation](README_EN_V1.2.1.md) | [GitHub](https://github.com/ArizeSky/Emby-In-One)

Multi-server Emby aggregation proxy — merges media libraries from multiple upstream Emby servers into a single unified endpoint accessible by any standard Emby client. (This version is the high-performance V1.3 Go backend refactor Pre-release).

## Demo Site

[Demo Site](https://emby.cothx.eu.cc/)
Emby server address: https://emby.cothx.eu.cc/
Username: admin
Password: 5T5xF4oMxcnrcCPA

## Preview

![Preview 1](https://cdn.nodeimage.com/i/D293pIQcFNx4gXkfskPbnXFzmgCQ1JPx.webp)
![Preview 2](https://cdn.nodeimage.com/i/iDAXrYaIXdm9efhwl2BtqJjRUmGfTSKU.webp)
![Preview 3](https://cdn.nodeimage.com/i/K4jhTTMjv8rkHYiPNbXKUC0kXIzAXgq0.webp)
![Preview 4](https://cdn.nodeimage.com/i/50eO6lJBev4Q5Zb1XhPgVH78kELtR1YK.webp)

> Image hosting provided by [NodeImage](https://www.nodeimage.com). Thanks for the support.

---

## Features Overview

- **Multi-Server Aggregation** — Merge movies, series, and search results across multiple servers into a unified view. Powered by Goroutine parallel requests, aggregation latency depends only on the slowest server.
- **Smart Deduplication & Prioritization** — Identical items are automatically merged while keeping all source versions available. Uses a 4-level metadata priority logic (Tag > Chinese > Length > Order) to pick the best display information.
- **Client Passthrough** — Isolates and passes your real client identity to the upstream server per proxy token to avoid cross-device conflicts. Features an exclusive 5-level persistence chain for auto-reconnects.
- **Advanced UA Spoofing** — Safely disguise as Infuse, or use `custom` mode to independently configure all 5 Emby client identity headers for any upstream server.
- **Network Proxy Pool** — Configure dedicated HTTP/HTTPS proxies separately for each upstream server, complete with a one-click connectivity tester.
- **Dual Playback Modes** — Setup upstreams with Proxy Mode (traffic routed through aggregator, hiding upstreams, supports HLS/seeking) or Redirect Mode (HTTP 302 to upstream direct links, saving proxy bandwidth).
- **Full Control & Operations** — Ships with a modernized SSH CLI menu and Web admin panel, featuring persistent logs and SQLite-backed ID mapping.
- **Security Hardening** — Built-in anti-bruteforce (IP locking), 0600 secure permissions for configs, atomic file writes, request body size limits, and graceful shutdown.

---

## Quick Installation

> **Notice for Legacy Node.js Deployment**: If you wish to deploy the V1.2.1 stable Node.js version, please navigate to the [Releases page](https://github.com/ArizeSky/Emby-In-One/releases) of this repository, download the V1.2.1 Source code archive, extract it, and run `bash install.sh`.

Deploying V1.3 (Go backend) via Docker on Linux is highly recommended.

### Method 1: One-Line Install Script (Recommended)

```bash
git clone https://github.com/ArizeSky/Emby-In-One.git
cd Emby-In-One
bash install.sh
```

The script automates Docker installation, assigns a random admin password, builds the Go image, and starts the service. To manage your server later, simply type `emby-in-one` in your SSH terminal to load the CLI menu.

### Method 2: Manual Docker Compose Deployment

1. Create project directories:
```bash
mkdir -p /opt/emby-in-one/{config,data}
cd /opt/emby-in-one
```
2. Copy the core files from this repository (including `go.mod`, `cmd/`, `internal/`, `public/`, `Dockerfile`, `docker-compose.yml`, etc.) into the directory.
3. Create the initial configuration `config/config.yaml`:
```yaml
server:
  port: 8096
  name: "Emby-In-One"

admin:
  username: "admin"
  password: "your-strong-password" # Automatically hashed after first boot

playback:
  mode: "proxy"

timeouts:
  api: 30000
  global: 15000
  login: 10000
  healthCheck: 10000
  healthInterval: 60000

proxies: []
upstream: []
```
4. Build and start up:
```bash
docker compose build
docker compose up -d
```

### Method 3: Direct Go Run (For Developers)

Requirements: Go 1.26+ with a C compiler (Debian/Ubuntu: `apt install build-essential`).
```bash
mkdir -p config data
# Create config.yaml inside /config as shown in Method 2
go test ./...
go run ./cmd/emby-in-one
```

**Default Access URLs**:
- Emby Client Endpoint: `http://Your_Server_IP:8096`
- Admin Panel: `http://Your_Server_IP:8096/admin`

---

## Advanced Configurations & Under The Hood

### Upstream Server Authentication

Each upstream server requires one of two auth methods:
1. **Username & Password**: Aggregator logs in via API to retrieve a Session Token.
2. **API Key**: Direct passwordless requests (Recommended method).

### Playback Mode Details

Can be set via global config or overridden per-server:
- **Proxy mode (proxy)**: Client stream traffic routes strictly through Emby-In-One. Video `.m3u8` payloads are rewritten with relative mapping paths to seamlessly mask raw upstream IPs. Best for hidden upstream setups or restrictive networks.
- **Redirect mode (redirect)**: Clients receive an HTTP `302` redirect containing the upstream stream token. Clients connect directly to the upstream media, saving aggregator bandwidth.

### Custom UA Spoofing (`spoofClient`)

To avoid upstream nodes detecting proxy agents, use these strategies:
- `passthrough`: Accurately forwards the client you're currently using (e.g., iPhone Infuse) to the upstream, complete with persistence and memory retention.
- `infuse`: Hardcoded disguise identical to the Infuse 7.7.1 client footprint.
- `custom`: Manually specify your own `User-Agent`, `X-Emby-Client` and the remaining 3 signature traits visible to upstreams.
- `none`: Use default aggregator identifier strings.

### Deduplication & ID Virtualization

Identical resources scattered across servers can be cleanly merged yet remain playable based on source choice:
- **Homologous Mapping**: After deduplication, combined movie/series data receives a single globally unique virtual UUID. A backend SQLite database `mappings.db` retains the relationships between this Virtual ID and all actual discrete IDs located across upstreams.
- **Media Version Consolidation**: When the system groups a movie (based on TMDB ID or Title matching), different server contents merge. Tapping the "Versions" selector in your player simply instructs Emby-In-One to reroute your stream to the backend server providing that specific version.

---

## Disclaimer

> **Notice**: This project communicates with upstream servers by simulating and masking Emby client behavior. There resides inherent risk of upstream operators or associated platforms detecting proxies and enforcing bans against your account or API Key. Utilization of this project equates to your self-assumption of these risks. The author bears zero responsibility for account bans, data loss, or other damages resulting from its use.

---

## Directory Structure (For Developers)

```text
Emby-In-One/
├── AGENTS.md
├── README.md
├── README_EN.md
├── Update.md
├── Update Plan.md
├── SECURITY.md          # Security guidelines
├── LICENSE
├── go.mod               # Go dependencies
├── cmd/                 # Application entrypoint
├── internal/            # Core proxy, dedup, and auth logic
├── third_party/sqlite/  # SQLite CGO codebase
├── public/              # Vue/Tailwind SPA admin panel 
├── src/                 # Legacy Node.js source code (Reference only)
├── tests/               # Legacy Node.js tests (Reference only)
├── package.json         # Legacy Node.js config (No boot needed)
├── Dockerfile           # Go runtime container build
├── docker-compose.yml 
├── install.sh           # Linux automated installer
└── emby-in-one-cli.sh   # Bash server management CLI
```

## License

GNU General Public License v3.0
