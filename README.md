# DOAL

**BitTorrent fake-seeding tool** with 18 anti-detection features. Written in Go — single binary, zero dependencies, ~10 MB, ~20 MB RAM.

> Fork of [JOAL](https://github.com/anthonyraymond/joal) (Java), entirely rewritten in Go for performance and portability.

---

## Installation

### Pre-built binaries

Download from [Releases](../../releases) — available for:
- Linux (amd64, arm64)
- macOS (amd64, arm64 / Apple Silicon)
- Windows (amd64, arm64)

### Build from source

```bash
git clone <this-repo>
cd doal-go
go build -ldflags="-s -w" -o doal .
```

Requires Go 1.23+.

---

## Usage

```bash
./doal --conf=. --port=5082 --path-prefix=doal --secret-token=your-secret
```

Then open **http://localhost:5082/doal/ui/**

### CLI flags

| Flag | Default | Description |
|------|---------|-------------|
| `--conf` | (required) | Path to config directory (contains `config.json`, `clients/`, `torrents/`) |
| `--port` | `5081` | Web server port |
| `--path-prefix` | `doal` | URL prefix (UI at `/{prefix}/ui/`) |
| `--secret-token` | (required) | Auth token for WebSocket (use `x` to disable) |

### Directory structure

```
your-config-dir/
├── config.json          # Main configuration
├── clients/             # 90+ BitTorrent client profiles (.client files)
├── torrents/            # Drop .torrent files here
│   ├── movie.torrent    # Torrent metadata
│   └── movie.mkv        # (Optional) Real file for SHA-1 piece verification
└── upload-stats.txt     # Auto-generated, tracks cumulative upload
```

---

## Configuration

All settings are configurable via the web UI or directly in `config.json`.

### Bandwidth

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `minUploadRate` | int | `100` | Minimum upload speed in kB/s per torrent |
| `maxUploadRate` | int | `1000` | Maximum upload speed in kB/s per torrent |
| `simultaneousSeed` | int | `5` | Number of torrents seeded simultaneously |
| `uploadRatioTarget` | float | `-1` | Target ratio (-1 = unlimited, >0 = auto-pause at ratio) |
| `minSpeedWhenNoLeechers` | int | `50` | Minimum speed in kB/s when 0 leechers (0 = disable) |
| `keepTorrentWithZeroLeechers` | bool | `false` | Continue seeding torrents with 0 leechers |
| `maxAnnounceFailures` | int | `5` | Remove torrent after N consecutive announce failures (0 = unlimited) |

### Anti-Detection

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `client` | string | `utorrent-3.5.0_43916.client` | BitTorrent client to emulate (90+ profiles) |
| `speedModel` | string | `ORGANIC` | Speed variation model (`ORGANIC` = realistic, `UNIFORM` = constant) |
| `announceJitterPercent` | int | `10` | Random variation on announce intervals (0-30%) |
| `peerResponseMode` | string | `BITFIELD` | How to respond to peer connections (`NONE`, `HANDSHAKE_ONLY`, `BITFIELD`, `FAKE_DATA`) |
| `perTorrentBandwidth` | bool | `true` | Each torrent gets independent speed (vs shared) |
| `enableBurstSpeed` | bool | `true` | Simulate upload speed bursts (1.5-3x) |
| `simulateDownload` | bool | `false` | Report download progress to tracker (left > 0 then completed) |
| `enablePortRotation` | bool | `false` | Change announced port every 30-60 min |
| `rotateClientOnRestart` | bool | `false` | Pick a random client profile on each start |
| `swarmAwareSpeed` | bool | `true` | Boost speed +20% for torrents with high leecher demand |

### Network

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `proxyEnabled` | bool | `false` | Route tracker announces through a proxy |
| `proxyType` | string | `socks5` | Proxy type (`socks5` or `http`) |
| `proxyUrl` | string | `""` | Proxy URL (e.g. `socks5://user:pass@host:1080`) |
| `announceIp` | string | `""` | Override IP reported to trackers (empty = auto-detect) |

### Schedule

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enableSchedule` | bool | `false` | Seed only during configured hours |
| `scheduleStartHour` | int | `22` | Hour to start seeding (0-23) |
| `scheduleEndHour` | int | `7` | Hour to stop seeding (0-23) |

---

## Anti-Detection Features (18)

| # | Feature | Description |
|---|---------|-------------|
| 1 | **Client Emulation** | 90+ client profiles with correct User-Agent, peer_id, key, query string order |
| 2 | **uTLS Fingerprint** | TLS handshake matches the emulated client (Chrome/Firefox/iOS profiles) |
| 3 | **Organic Speed** | Random walk with momentum, Gaussian noise, micro-jitter, occasional drops |
| 4 | **Speed Warmup** | Gradual ramp from 0 to 100% over 60 seconds after start |
| 5 | **Burst Speed** | Random speed spikes (1.5-3x) simulating new leecher arrival |
| 6 | **Upload Byte Jitter** | Random noise on accumulated bytes to avoid exact multiples |
| 7 | **Announce Jitter** | Random variation on tracker announce intervals |
| 8 | **PeerWire Protocol** | Responds to incoming peer connections with valid BT handshake |
| 9 | **BEP 10 Extension** | Extension protocol handshake (ut_metadata, ut_pex) |
| 10 | **PEX Messages** | Periodic Peer Exchange messages to appear as real peer |
| 11 | **DHT Node** | Minimal DHT node responding to ping/find_node/get_peers |
| 12 | **Keep-Alive** | 120-second keep-alive messages on peer connections |
| 13 | **Reserved Bytes** | Correct DHT + Extension Protocol + Fast Extension bits in handshake |
| 14 | **SimulateDownload** | Reports download progress with correct left/downloaded/completed events |
| 15 | **Port Rotation** | Changes announced port every 30-60 minutes |
| 16 | **Client Rotation** | Switches to a different client profile on each restart |
| 17 | **SHA-1 Piece Serving** | Serves real file data for piece verification (when data file is present) |
| 18 | **Announce Stagger** | Spreads initial announces over 0-15 seconds to avoid timestamp clustering |

---

## Web UI

Modern dark-themed dashboard built with Tailwind CSS, Chart.js, and Lucide icons.

- **Real-time speed graph** with organic variations
- **Per-torrent stats** (speed, upload, seeders, leechers)
- **Per-tracker stats** with pause/resume per tracker
- **Pause/resume individual torrents**
- **Drag & drop torrent upload**
- **Dark/Light mode toggle**
- **Desktop notifications**
- **Auto-connect** (no login modal needed with `--secret-token=x`)
- **Persistent history** across page refreshes (sessionStorage)

---

## SHA-1 Piece Verification

To enable real piece serving (passes tracker SHA-1 checks):

1. Place the real data file in `torrents/` with the same base name as the `.torrent`:
   ```
   torrents/
     MyMovie.2024.1080p.torrent    # torrent metadata
     MyMovie.2024.1080p.mkv        # actual file content
   ```
2. DOAL auto-detects the pair on start (matched by file size)
3. When a peer requests a piece, DOAL serves the real bytes instead of random data

Without the data file, DOAL falls back to random bytes (FAKE_DATA mode).

---

## Development

```bash
# Run tests (255 tests across 7 packages)
go test ./... -count=1 -timeout=120s

# Run with race detector
go test ./... -race

# Lint
go vet ./...

# Build optimized binary
go build -ldflags="-s -w" -o doal .
```

### Project structure

```
doal-go/
├── main.go                 # Engine, CLI, start/stop/pause/rotate
├── config/config.go        # 22-field JSON config + validation
├── torrent/
│   ├── parser.go           # Bencode parser, .torrent files
│   └── watcher.go          # fsnotify file watcher
├── bandwidth/
│   ├── dispatcher.go       # Speed computation, upload accumulation
│   ├── organic_speed.go    # Organic speed provider (random walk)
│   └── random_speed.go     # Uniform random speed provider
├── announce/
│   ├── announcer.go        # HTTP tracker announce
│   ├── scheduler.go        # Multi-torrent scheduler + jitter
│   ├── client_emulator.go  # 90+ client profiles
│   └── tls.go              # uTLS fingerprint spoofing
├── peerwire/
│   ├── server.go           # BT handshake + bitfield + BEP10 + PEX
│   └── piececache.go       # SHA-1 verified piece serving
├── dht/dht.go              # Minimal DHT node (BEP 5)
├── persistence/stats.go    # Upload stats file persistence
└── web/
    ├── server.go            # HTTP + WebSocket server
    ├── stomp.go             # STOMP 1.2 protocol
    ├── handlers.go          # Message routing + broadcast
    └── static/index.html    # Embedded frontend (Tailwind + Chart.js)
```

---

## Original Project

This is a complete rewrite of [JOAL](https://github.com/anthonyraymond/joal) by Anthony Raymond.

**Key differences from JOAL:**
- Rewritten from Java to Go (9 MB binary vs 33 MB JAR + 200 MB JVM)
- 18 anti-detection features (vs 6 in original)
- uTLS fingerprint spoofing
- DHT/PEX participation
- SHA-1 piece verification
- Proxy support (SOCKS5/HTTP)
- Modern Tailwind UI (vs legacy React)
- Per-torrent/tracker pause/resume
- Torrent rotation
- Upload ratio enforcement

---

## License

MIT
