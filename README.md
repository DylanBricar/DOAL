# DOAL

**BitTorrent tracker test client** with protocol and traffic-simulation features. Written in Go — single binary, zero dependencies, ~10 MB, ~20 MB RAM.

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
./doal --conf=. --port=5082 --path-prefix=doal --secret-token=x
```

Then open **http://localhost:5082/** (auto-redirects to the UI)

### CLI flags

| Flag | Default | Description |
|------|---------|-------------|
| `--conf` | (required) | Path to config directory (contains `config.json`, `clients/`, `torrents/`) |
| `--port` | `5081` | Web server port |
| `--path-prefix` | `doal` | URL prefix (UI at `/{prefix}/ui/`) |
| `--secret-token` | required | WebSocket auth token. The explicit local mode `x` disables authentication and binds the Web UI to `127.0.0.1` only. A real token allows network listening. |

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
| `keepTorrentWithZeroLeechers` | bool | `false` | Continue seeding torrents with 0 leechers |
| `maxAnnounceFailures` | int | `5` | Remove torrent after N consecutive announce failures (0 = unlimited) |

### Traffic simulation

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `client` | string | `utorrent-3.5.0_43916.client` | BitTorrent client to emulate (90+ profiles) |
| `speedModel` | string | `ORGANIC` | Speed variation model (`ORGANIC` = realistic, `UNIFORM` = constant) |
| `announceJitterPercent` | int | `10` | Random variation on announce intervals (0-30%) |
| `peerResponseMode` | string | `BITFIELD` | How to respond to peer connections (`NONE`, `HANDSHAKE_ONLY`, `BITFIELD`, `FAKE_DATA`) |
| `perTorrentBandwidth` | bool | `true` | Each torrent gets independent speed (vs shared) |
| `enableBurstSpeed` | bool | `true` | Simulate upload speed bursts (1.5-3x) |
| `simulateDownload` | bool | `true` | Report a conserved download lifecycle (`downloaded + left = size`) with warmup, stalls and a single completed event |
| `rotateClientOnRestart` | bool | `false` | Pick a random client profile on each start |
| `swarmAwareSpeed` | bool | `true` | Boost speed +20% for torrents with high leecher demand |

### Network

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `proxyEnabled` | bool | `false` | Route tracker announces through a proxy |
| `proxyType` | string | `socks5` | Proxy type (`socks5` or `http`) |
| `proxyUrl` | string | `""` | Proxy URL (e.g. `socks5://user:pass@host:1080`) |
| `announceIp` | string | `""` | Override IP reported to trackers (empty = auto-detect) |
| `enablePieceProxy` | bool | `false` | On-demand piece proxy: leech a requested piece live from a real seed, SHA-1 verify it, then serve it. Only meaningful in `FAKE_DATA` mode. See below. |
| `dhtBootstrapNodes` | string[] | `[]` | Explicit DHT entry points; any valid DNS hostname or IP address with a port is accepted |
| `enableLabSybilRing` | bool | `false` | Enable matched counterparty accounting inside the configured tracker lab |
| `labSybilPeers` | int | `0` | Counterparties in the lab ring; must be 2-8 when enabled |

Tracker announces accept any domain or IP address over HTTP(S). Malformed URLs,
embedded credentials and unsupported schemes remain rejected; redirects are
validated by the same rules. DHT traffic accepts any explicitly configured
valid `host:port` endpoint. The lab ring is disabled by default and cannot
exceed eight counterparties.

### Schedule

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enableSchedule` | bool | `false` | Seed only during configured hours |
| `scheduleStartHour` | int | `22` | Hour to start seeding (0-23) |
| `scheduleEndHour` | int | `7` | Hour to stop seeding (0-23) |

---

## Protocol and traffic simulation features

| # | Feature | Description |
|---|---------|-------------|
| 1 | **Client Emulation** | 90+ client profiles with correct User-Agent, peer_id, key, query string order |
| 2 | **uTLS Fingerprint** | Browser presets or an OpenSSL-style libtorrent ClientHello, with HTTP/1.1-only ALPN |
| 3 | **Organic Speed** | Independent heavy-tailed random walk, momentum, micro-jitter and real zero plateaus per torrent |
| 4 | **Speed Warmup** | Per-torrent randomized delay and 45-180 second ramp from a true zero |
| 5 | **Burst Speed** | Per-torrent decorrelated speed spikes (1.5-3x) |
| 6 | **Upload Byte Jitter** | Random noise on accumulated bytes to avoid exact multiples |
| 7 | **Announce Jitter** | Random variation on tracker announce intervals |
| 8 | **PeerWire Protocol** | Responds to incoming peer connections with valid BT handshake |
| 9 | **BEP 10 Extension** | Extension protocol handshake (ut_metadata, ut_pex) |
| 10 | **PEX Messages** | Negotiated, rate-limited PEX containing newly observed live peers |
| 11 | **DHT Node** | Private participatory DHT with ping/find_node/get_peers/announce_peer and token validation |
| 12 | **Keep-Alive** | 120-second keep-alive messages on peer connections |
| 13 | **Reserved Bytes** | Correct DHT + Extension Protocol + Fast Extension bits in handshake |
| 14 | **SimulateDownload** | Reports download progress with correct left/downloaded/completed events |
| 15 | **Client Rotation** | Switches to a different client profile on each restart |
| 16 | **SHA-1 Piece Serving** | Serves real file data for piece verification (when data file is present) |
| 17 | **Announce Stagger** | Spreads initial announces over 0-15 seconds to avoid timestamp clustering |
| 18 | **On-Demand Piece Proxy** | When probed for a piece it lacks locally, leeches that piece live from a real seed, SHA-1 verifies it, then serves it (`enablePieceProxy`, `FAKE_DATA` mode) |
| 19 | **Metadata Exchange** | Serves exact torrent info bytes through negotiated `ut_metadata` blocks |
| 20 | **Matched Lab Ring** | Optional bounded counterparties whose downloads exactly match newly observed upload bytes |

---

## On-Demand Piece Proxy (isolated lab capability)

Peer data is fail-closed. Without a verified local file, the server advertises
an empty bitfield and rejects unavailable requests instead of manufacturing
random bytes.

With `enablePieceProxy: true` (and `peerResponseMode: FAKE_DATA`), on a cache
miss DOAL instead:

1. picks a real seed from the tracker's announce response (compact peer list),
2. connects to it and leeches the requested piece,
3. checks the piece against the torrent's own SHA-1 hash,
4. stores it in a bounded cache and serves only the verified bytes.

The proxy is disabled by default and restricted to the configured lab scope. It
cannot provide a piece absent from every upstream source; that request is
rejected. Its memory cache is globally bounded and cleared during shutdown.

## Web UI

Modern dark-themed dashboard built with Tailwind CSS, Chart.js, and Lucide icons.

- **Real-time speed graph** with organic variations
- **Per-torrent stats** (speed, upload, seeders, leechers)
- **Per-tracker stats** with pause/resume per tracker
- **Pause/resume individual torrents**
- **Drag & drop torrent upload**
- **Dark/Light mode toggle**
- **Desktop notifications**
- **Auto-connect** (no login required by default)
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
2. DOAL verifies the complete file size and every piece SHA-1 before registration
3. When a peer requests a valid block, DOAL serves bytes from that verified file

Without a verified data source, DOAL advertises no available pieces and rejects
piece requests.

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
- Expanded protocol and traffic-simulation coverage
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
