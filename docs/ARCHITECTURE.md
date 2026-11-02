# JellyProxy: Architecture & Specification

## 1. Overview & Problem Statement
Hosting a media server like Jellyfin from a residential network often faces severe upstream bandwidth limitations (e.g. 2 MB/s / 16 Mbps). When remote clients stream content:
- Multiple concurrent streams saturate the upstream link.
- Watch parties (SyncPlay) consume duplicate bandwidth for identical content.
- High-bitrate files cannot be streamed without stuttering.
- UI assets (posters, backdrops, metadata) load slowly over limited upload.

JellyProxy solves this by establishing a high-performance **Edge Caching, Stream-Splitting, and Adaptive Bandwidth Proxy** hosted on a cloud/VPS node (2 Gbps) paired with a native companion plugin on the home Jellyfin server.

---

## 2. Core Components

### A. JellyProxy Edge Service (VPS Docker Container)
- **Language**: Go (high concurrency, lightweight memory footprint).
- **Functionality**:
  1. **Transparent Reverse Proxy**: Zero-overhead pass-through for auth, standard API, and WebSockets (`/socket`).
  2. **Range & Chunk Multiplexer**: Slices video streams into uniform HLS/fMP4 chunks (`.m4s`) with an LRU cache on disk.
  3. **SyncPlay Multiplexer**: Multiple remote clients watching the same stream receive bytes from the VPS cache while pulling only a single stream from home.
  4. **Dynamic Fair-Share Bandwidth Shaper**: Arbitrates available home upstream bandwidth dynamically among active streams.
  5. **Pause & Prefetch Engine**: Sniffs playback progress (`/Sessions/Playing/*`); continues buffering on pause to prevent future buffer underruns.
  6. **Movie Night Pre-seeding**: Asynchronously pulls full files to VPS storage ahead of scheduled watch events.

### B. JellyProxy Companion Plugin (Jellyfin .NET 9 Plugin)
- **Language**: C# (.NET 9 SDK).
- **Functionality**:
  1. **Handshake & Pairing**: Secure token-based registration with the proxy via `/jellyproxy/api/handshake`.
  2. **Playback Negotiation Arbitrator**: Intercepts `POST /PlaybackInfo` to adapt device profiles and enforce master playlist ladders.
  3. **Real-time Event Webhooks**: Signals library updates, watch-state changes, and next-episode IDs to the proxy for cache invalidation and prefetching.
  4. **Native Dashboard UI**: Configuration panel embedded directly in the Jellyfin admin dashboard.

---

## 3. Network Flow & Routing Optimization

### Traditional Path (Double Hop - High Latency)
```
[Client] ──HTTPS──> [Traefik VPS] ──HTTP──> [JellyProxy] ──HTTPS──> [Traefik VPS] ──WG──> [Newt/HomeServer]
```
*Disadvantages*: Double TLS termination, double middleware overhead, hairpin routing.

### Optimized Edge Path (Zero Loopback - Maximum Throughput)
```
[Client]
   │ HTTPS (443)
   ▼
[Traefik VPS (Reverse Proxy)]
   │ 127.0.0.1:8096 (Local Memory Loopback)
   ▼
[JellyProxy Engine]
   │ WireGuard Tunnel (100.89.128.4:52905)
   ▼
[HomeServer Jellyfin (Direct Raw Stream)]
```

---

## 4. Feature Matrix & Implementation Phases

| Phase | Milestone | Key Features |
| :--- | :--- | :--- |
| **Phase 1** | Foundation | Go proxy passthrough, .NET plugin, handshake pairing, CI/CD manifest publishing. |
| **Phase 2** | Media Engine | HLS fMP4 chunk caching, Range slice multiplexing, audio/video track splitting. |
| **Phase 3** | ABR & Shaper | Dynamic bandwidth allocation, buffer-aware bursting, pause prefetching. |
| **Phase 4** | Watch Parties | SyncPlay single-pull optimization, Movie Night pre-seeding pipeline. |
