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
   │ WireGuard Tunnel (198.51.100.10:52905)
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


---

## 5. Bandwidth Policy (how the spec cases are implemented)

| Case | Behaviour | Where |
| :--- | :--- | :--- |
| 1 | Demands that fit in X are all satisfied (work-conserving max-min fairness). | `abr.reallocate` |
| 2 | A request above X is capped to X (`maxStreamingBitrate` rewritten in `PlaybackInfo`, variants above the ceiling removed from `master.m3u8`). | `handlers/playbackinfo.go` |
| 3, 6 | Over-subscription is taken from the greedy stream first; equal demands split evenly. | `abr.reallocate` |
| 4 | Same title + same codec/audio/subtitle signature = one pipe, one upstream pull. | `abr.Signature` |
| 5 | A different audio language is a different signature, so both pipes degrade to X/2. Muxing one video + two audios is **not** implemented. | `abr.Signature` |
| 7 | Clients converge on one codec; fairness is charged per quality unit (h264 1.0, hevc 0.65, av1 0.55), so the better codec uses fewer real Mb/s. | `abr/codec.go` |
| 8 | A cold pipe gets 90% of X for 10s (fast-start). | `abr.computePullsLocked` |
| 9 | Segments are cached by session-independent key; a later viewer reads them from the VPS at zero upload. | `cache.BuildKey` |
| 10 | `Encode` subtitle profiles for text formats are removed and WebVTT is offered as external, so Jellyfin converts ASS/SSA/SRT. Image subtitles (PGS) still burn in and get a private pipe. | `handlers/playbackinfo.go` |
| 11 | Prefetch and pre-seed use `BackgroundWait`: whole link when idle, 5% of X while anyone plays. | `abr.BackgroundWait` |
| 12 | A `seek` report grants the seeking pipe a 4s burst. | `playback.HandleReport` |

## 6. Dashboard & Configuration

`https://<domain>/proxy/` serves the live connection schema, statistics, and runtime configuration panel (authenticated with `SHARED_SECRET`). At startup the proxy probes the upstream and only mounts the UI on a path Jellyfin answers 404 for (falling back to `/jellyproxy`).

All operational settings (upload speed, headroom, policy, min bitrate, single-client ceiling, fast-start burst, seek borrow, background prefetch floor, supported encoders, and cache storage quota) are configured via either the **Jellyfin companion plugin** or the **`/proxy/` web dashboard**, and are persisted to `CACHE_DIR/settings.json`.
