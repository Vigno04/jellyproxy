# JellyProxy

Smart edge caching, bandwidth multiplexing, and adaptive reverse proxy for **Jellyfin**, designed specifically for low-upload home servers paired with high-bandwidth VPS / cloud nodes.

---

## ⚡ The Problem

When hosting Jellyfin from a home server with limited upload speed (e.g., 2 MB/s / 16 Mbps):
- Multiple concurrent streams from outside your network quickly saturate the link.
- Two users watching the same movie or episode (e.g., via SyncPlay) consume double the home upload.
- Loading posters, backdrops, and library metadata over slow upstream links causes client UI lag.

## 🚀 The Solution: JellyProxy

JellyProxy splits the workload intelligently between your **HomeServer (Jellyfin Master)** and a **VPS Edge Node**:

1. **JellyProxy Edge (Docker Container)**: Sits on your high-bandwidth VPS (e.g. 2 Gbps), acting as a transparent reverse proxy for remote clients.
2. **JellyProxy Companion Plugin (Jellyfin .NET Plugin)**: Installed on your Jellyfin master server. Pairs with the proxy, orchestrates playback negotiation, and synchronizes session state.

```
[ Remote Clients: TV / Mobile / Browser ]
                   │
                   ▼ (High-speed Cloud link: 2 Gbps)
        ┌─────────────────────┐
        │  JellyProxy (VPS)   │ ◄─── Edge Caching & ABR Orchestrator
        └─────────────────────┘
                   │
                   ▼ (Home Upload link: ~2 MB/s)
        ┌─────────────────────┐
        │  Jellyfin Master    │ ◄─── Companion Plugin
        └─────────────────────┘
```

---

## 📦 Current Features (v0.1.0)

- [x] **Transparent Reverse Proxy**: Full HTTP/1.1 and WebSocket passthrough (`/socket`) with zero overhead.
- [x] **Plugin-Proxy Handshake Protocol**: Authenticated handshake over `/jellyproxy/api/handshake` via shared secret token.
- [x] **Native Jellyfin Dashboard Configuration**: Manage edge URL and credentials directly from the Jellyfin Admin Dashboard with one-click connection testing.
- [x] **Zero Transcoding on VPS**: Proxy runs as a lightweight Go binary without GPU requirements.

---

## 🛠️ Quick Start

### 1. Deploy the Proxy on VPS

Run with Docker Compose:

```yaml
services:
  jellyproxy:
    image: ghcr.io/jellyproxy-project/jellyproxy:latest # or build locally
    container_name: jellyproxy
    restart: unless-stopped
    ports:
      - "8080:8080"
    environment:
      - PORT=8080
      - UPSTREAM_URL=http://your-homeserver-tunnel-ip:8096
      - SHARED_SECRET=your_secure_shared_secret
      - LOG_LEVEL=info
```

```bash
docker compose up -d
```

Verify proxy health:
```bash
curl http://localhost:8080/jellyproxy/api/status
```

### 2. Install the Companion Plugin

1. Download `JellyProxy.zip` from [Releases](https://github.com/jellyproxy-project/jellyproxy/releases) or build from `plugin/JellyProxy/`.
2. Extract into your Jellyfin `plugins/JellyProxy/` folder.
3. Restart Jellyfin.
4. Go to **Dashboard -> Plugins -> JellyProxy**, enter your **Proxy Edge URL** and **Shared Secret**, then click **Test Connection & Pair**.

---

## 🗺️ Roadmap

- **Smart Chunk Caching (HLS Transmuxing)**: Uniform slicing of Direct Play streams into static `.m4s` chunks with LRU disk cache.
- **SyncPlay Multiplexing**: Multiple remote clients stream the same movie from the VPS cache while only 1 stream is pulled from home.
- **Adaptive Bandwidth Shaper**: Dynamic fair allocation of home upload across active sessions.
- **Pause & Prefetch Engine**: When a user pauses, continue buffering the file in the background on the VPS to prevent future stutter.
- **Movie Night Pre-seeding**: Asynchronously stage selected movies onto the VPS before group watch parties.

---

## 📄 License

GPL-3.0 License.
