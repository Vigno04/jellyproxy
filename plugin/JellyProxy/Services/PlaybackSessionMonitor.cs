using System;
using System.Collections.Concurrent;
using System.Globalization;
using System.Linq;
using System.Net.Http;
using System.Net.Mime;
using System.Text;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;
using JellyProxy.Configuration;
using MediaBrowser.Controller.Configuration;
using MediaBrowser.Controller.Entities;
using MediaBrowser.Controller.Entities.TV;
using MediaBrowser.Controller.Library;
using MediaBrowser.Controller.Session;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;

namespace JellyProxy.Services;

/// <summary>
/// Background service that monitors Jellyfin playback sessions (Start, Progress, Pause, Seek, Stop)
/// and synchronizes state with JellyProxy for intelligent prefetch and bandwidth shaping.
/// </summary>
public class PlaybackSessionMonitor : IHostedService, IDisposable
{
    private readonly ISessionManager _sessionManager;
    private readonly IServerConfigurationManager _serverConfigurationManager;
    private readonly IHttpClientFactory _httpClientFactory;
    private readonly ILogger<PlaybackSessionMonitor> _logger;
    private readonly ConcurrentDictionary<string, SessionStateTracker> _activeSessions = new();
    private CancellationTokenSource? _cts;
    private bool _disposed;

    /// <summary>
    /// Initializes a new instance of the <see cref="PlaybackSessionMonitor"/> class.
    /// </summary>
    public PlaybackSessionMonitor(
        ISessionManager sessionManager,
        IServerConfigurationManager serverConfigurationManager,
        IHttpClientFactory httpClientFactory,
        ILogger<PlaybackSessionMonitor> logger)
    {
        _sessionManager = sessionManager;
        _serverConfigurationManager = serverConfigurationManager;
        _httpClientFactory = httpClientFactory;
        _logger = logger;
    }

    /// <inheritdoc />
    public Task StartAsync(CancellationToken cancellationToken)
    {
        _sessionManager.PlaybackStart += OnPlaybackStart;
        _sessionManager.PlaybackProgress += OnPlaybackProgress;
        _sessionManager.PlaybackStopped += OnPlaybackStopped;
        _sessionManager.SessionEnded += OnSessionEnded;
        _cts = new CancellationTokenSource();
        _ = PeriodicSyncLoopAsync(_cts.Token);
        _logger.LogInformation("PlaybackSessionMonitor started and subscribed to session events.");
        return Task.CompletedTask;
    }

    private async Task PeriodicSyncLoopAsync(CancellationToken token)
    {
        await SyncSettingsWithProxyAsync(initial: true).ConfigureAwait(false);
        using var timer = new PeriodicTimer(TimeSpan.FromSeconds(60));
        while (await timer.WaitForNextTickAsync(token).ConfigureAwait(false))
        {
            await SyncSettingsWithProxyAsync(initial: false).ConfigureAwait(false);
            BroadcastActiveSessions();
        }
    }

    private void BroadcastActiveSessions()
    {
        try
        {
            foreach (var session in _sessionManager.Sessions)
            {
                if (session?.NowPlayingItem == null)
                {
                    continue;
                }
                var itemId = session.NowPlayingItem.Id.ToString("N");
                var posTicks = session.PlayState?.PositionTicks ?? 0;
                var totalTicks = session.NowPlayingItem.RunTimeTicks;
                var isPaused = session.PlayState?.IsPaused ?? false;
                var playMethod = session.PlayState?.PlayMethod?.ToString();
                var playSessionId = _activeSessions.TryGetValue(session.Id ?? string.Empty, out var tracked) ? tracked.PlaySessionId : null;
                var itemName = FormatDisplayTitle(session.FullNowPlayingItem, session.NowPlayingItem.Name, session.NowPlayingItem.SeriesName, session.NowPlayingItem.ParentIndexNumber, session.NowPlayingItem.IndexNumber);
                SendPlaybackReport(session, itemName, itemId, playSessionId, posTicks, totalTicks, isPaused, "progress", playMethod);
            }
        }
        catch (Exception ex)
        {
            _logger.LogDebug(ex, "Periodic active session broadcast failed");
        }
    }

    private async Task SyncSettingsWithProxyAsync(bool initial)
    {
        try
        {
            if (initial)
            {
                await Task.Delay(TimeSpan.FromSeconds(2)).ConfigureAwait(false);
            }
            var config = Plugin.Instance?.Configuration;
            if (config == null || string.IsNullOrWhiteSpace(config.ProxyUrl))
            {
                return;
            }

            var proxyUrl = config.ProxyUrl.TrimEnd('/');
            var client = _httpClientFactory.CreateClient("JellyProxy");
            client.Timeout = TimeSpan.FromSeconds(10);

            var encoders = EncoderDetector.ResolveEncoders(_serverConfigurationManager, config, out _);

            if (!initial)
            {
                var capsReq = new HttpRequestMessage(HttpMethod.Post, $"{proxyUrl}/jellyproxy/api/abr/capabilities")
                {
                    Content = new StringContent(JsonSerializer.Serialize(new { encoders }), Encoding.UTF8, MediaTypeNames.Application.Json)
                };
                if (!string.IsNullOrWhiteSpace(config.SharedSecret))
                {
                    capsReq.Headers.TryAddWithoutValidation("X-JellyProxy-Secret", config.SharedSecret);
                }
                await client.SendAsync(capsReq).ConfigureAwait(false);
                return;
            }

            var handshakeUrl = $"{proxyUrl}/jellyproxy/api/handshake";
            var payload = new
            {
                secret = config.SharedSecret,
                server_name = _serverConfigurationManager.Configuration?.ServerName ?? "Jellyfin",
                server_id = _serverConfigurationManager.Configuration?.ServerName ?? "Jellyfin",
                plugin_version = "0.6.0",
                jellyfin_version = "10.11.x",
                max_storage_gb = config.MaxStorageGb,
                encoders = encoders,
                abr = new
                {
                    enabled = config.EnableBandwidthShaping,
                    uploadSpeedBps = (long)config.UploadSpeedMbps * 1_000_000L,
                    headroomPercent = config.HeadroomPercent,
                    policy = config.ShaperPolicy,
                    allowDegradingExistingStreams = config.AllowDegradingExisting,
                    minBitrateBps = (long)config.MinBitrateMbps * 1_000_000L,
                    singleClientCeilingBps = (long)config.SingleClientCeilingMbps * 1_000_000L,
                    fastStartSeconds = config.FastStartSeconds,
                    fastStartSharePercent = config.FastStartSharePercent,
                    seekBorrowSeconds = config.SeekBorrowSeconds,
                    backgroundFloorPercent = config.BackgroundFloorPercent
                }
            };

            var req = new HttpRequestMessage(HttpMethod.Post, handshakeUrl)
            {
                Content = new StringContent(JsonSerializer.Serialize(payload), Encoding.UTF8, MediaTypeNames.Application.Json)
            };
            if (!string.IsNullOrWhiteSpace(config.SharedSecret))
            {
                req.Headers.TryAddWithoutValidation("X-JellyProxy-Secret", config.SharedSecret);
            }
            await client.SendAsync(req).ConfigureAwait(false);
        }
        catch (Exception ex)
        {
            _logger.LogDebug(ex, "Settings sync with JellyProxy skipped or failed");
        }
    }

    /// <inheritdoc />
    public Task StopAsync(CancellationToken cancellationToken)
    {
        _cts?.Cancel();
        Unsubscribe();
        _activeSessions.Clear();
        _logger.LogInformation("PlaybackSessionMonitor stopped and unsubscribed from session events.");
        return Task.CompletedTask;
    }

    private void OnPlaybackStart(object? sender, PlaybackProgressEventArgs e)
    {
        if (e?.Session == null)
        {
            return;
        }

        var sessionId = e.Session.Id ?? string.Empty;
        var itemId = e.Item?.Id.ToString("N") ?? string.Empty;
        var posTicks = e.PlaybackPositionTicks ?? 0;

        _activeSessions[sessionId] = new SessionStateTracker
        {
            ItemId = itemId,
            PlaySessionId = e.PlaySessionId ?? string.Empty,
            IsPaused = false,
            PositionTicks = posTicks,
            LastReportTime = DateTimeOffset.UtcNow,
            LastStateChangeTime = DateTimeOffset.UtcNow
        };

        var title = FormatDisplayTitle(e.Item, e.Item?.Name, e.Session.NowPlayingItem?.SeriesName, e.Item?.ParentIndexNumber, e.Item?.IndexNumber);
        SendPlaybackReport(e.Session, title, itemId, e.PlaySessionId, posTicks, e.Item?.RunTimeTicks, false, "start", e.Session.PlayState?.PlayMethod.ToString());
    }

    private void OnPlaybackProgress(object? sender, PlaybackProgressEventArgs e)
    {
        if (e?.Session == null)
        {
            return;
        }

        var sessionId = e.Session.Id ?? string.Empty;
        var currentTicks = e.PlaybackPositionTicks ?? 0;
        var isPaused = e.IsPaused;
        var itemId = e.Item?.Id.ToString("N") ?? string.Empty;
        var now = DateTimeOffset.UtcNow;

        var state = _activeSessions.GetOrAdd(sessionId, _ => new SessionStateTracker
        {
            ItemId = itemId,
            PlaySessionId = e.PlaySessionId ?? string.Empty,
            IsPaused = isPaused,
            PositionTicks = currentTicks,
            LastReportTime = now,
            LastStateChangeTime = now
        });

        bool isPauseTransition = isPaused != state.IsPaused;
        bool isItemChange = !string.IsNullOrEmpty(itemId) && itemId != state.ItemId;

        long elapsedTicks = (long)(now - state.LastReportTime).TotalMilliseconds * 10000;
        long tickDelta = Math.Abs(currentTicks - state.PositionTicks);
        bool isSeek = !isPaused && !state.IsPaused && tickDelta > (elapsedTicks + 50_000_000);

        string? eventName = null;
        if (isItemChange)
        {
            eventName = "start";
        }
        else if (isPauseTransition)
        {
            eventName = isPaused ? "pause" : "unpause";
        }
        else if (isSeek)
        {
            eventName = "seek";
        }
        else if (now - state.LastReportTime >= TimeSpan.FromSeconds(15))
        {
            eventName = "progress";
        }

        if (eventName != null)
        {
            state.ItemId = itemId;
            state.PlaySessionId = e.PlaySessionId ?? state.PlaySessionId;
            state.IsPaused = isPaused;
            state.PositionTicks = currentTicks;
            state.LastReportTime = now;
            if (isPauseTransition || isSeek || isItemChange)
            {
                state.LastStateChangeTime = now;
            }

            var title = FormatDisplayTitle(e.Item, e.Item?.Name, e.Session.NowPlayingItem?.SeriesName, e.Item?.ParentIndexNumber, e.Item?.IndexNumber);
            SendPlaybackReport(e.Session, title, itemId, e.PlaySessionId, currentTicks, e.Item?.RunTimeTicks, isPaused, eventName, e.Session.PlayState?.PlayMethod.ToString());
        }
    }

    private void OnPlaybackStopped(object? sender, PlaybackStopEventArgs e)
    {
        if (e?.Session == null)
        {
            return;
        }

        var sessionId = e.Session.Id ?? string.Empty;
        _activeSessions.TryRemove(sessionId, out _);

        var itemId = e.Item?.Id.ToString("N") ?? string.Empty;
        var posTicks = e.PlaybackPositionTicks ?? 0;
        var title = FormatDisplayTitle(e.Item, e.Item?.Name, e.Session.NowPlayingItem?.SeriesName, e.Item?.ParentIndexNumber, e.Item?.IndexNumber);

        SendPlaybackReport(e.Session, title, itemId, e.PlaySessionId, posTicks, e.Item?.RunTimeTicks, false, "stop", e.Session.PlayState?.PlayMethod.ToString());
    }

    private void OnSessionEnded(object? sender, SessionEventArgs e)
    {
        if (e?.SessionInfo == null)
        {
            return;
        }

        var sessionId = e.SessionInfo.Id ?? string.Empty;
        if (_activeSessions.TryRemove(sessionId, out var state))
        {
            SendPlaybackReport(e.SessionInfo, null, state.ItemId, state.PlaySessionId, state.PositionTicks, null, false, "stop", null);
        }
    }

    private static string? FormatDisplayTitle(BaseItem? item, string? name, string? seriesName, int? season, int? episode)
    {
        if (item is Episode ep)
        {
            seriesName ??= ep.SeriesName;
            season ??= ep.ParentIndexNumber;
            episode ??= ep.IndexNumber;
        }
        if (!string.IsNullOrWhiteSpace(seriesName))
        {
            if (season.HasValue && episode.HasValue)
            {
                return string.Format(CultureInfo.InvariantCulture, "{0} S{1:00}E{2:00} · {3}", seriesName, season.Value, episode.Value, name ?? "Episode");
            }
            return $"{seriesName} · {name}";
        }
        return name;
    }

    private void SendPlaybackReport(
        SessionInfo session,
        string? itemName,
        string itemId,
        string? playSessionId,
        long positionTicks,
        long? totalTicks,
        bool isPaused,
        string eventType,
        string? playMethod)
    {
        _ = Task.Run(async () =>
        {
            try
            {
                var config = Plugin.Instance?.Configuration;
                if (config == null || string.IsNullOrWhiteSpace(config.ProxyUrl) || !config.AutoSyncPlayback)
                {
                    return;
                }

                var proxyUrl = config.ProxyUrl.TrimEnd('/');
                var reportUrl = $"{proxyUrl}/jellyproxy/api/playback/report";

                var client = _httpClientFactory.CreateClient("JellyProxy");
                client.Timeout = TimeSpan.FromSeconds(5);

                var tr = session.TranscodingInfo;
                var ps = session.PlayState;
                var videoCodec = tr?.VideoCodec;
                if (string.IsNullOrWhiteSpace(videoCodec))
                {
                    videoCodec = session.NowPlayingItem?.MediaStreams?
                        .FirstOrDefault(s => s.Type == MediaBrowser.Model.Entities.MediaStreamType.Video)?.Codec;
                }
                var audioCodec = tr?.AudioCodec;

                var payload = new
                {
                    secret = config.SharedSecret,
                    session_id = session.Id,
                    device_id = session.DeviceId,
                    device_name = session.DeviceName,
                    client_name = session.Client,
                    user_id = session.UserId.ToString("N"),
                    user_name = session.UserName,
                    item_id = itemId,
                    item_name = itemName,
                    play_session_id = playSessionId,
                    position_ticks = positionTicks,
                    total_ticks = totalTicks ?? 0,
                    is_paused = isPaused,
                    event_type = eventType,
                    play_method = playMethod ?? "DirectPlay",
                    video_codec = videoCodec ?? string.Empty,
                    audio_codec = audioCodec ?? string.Empty,
                    transcode_bitrate = (long)(tr?.Bitrate ?? 0),
                    transcode_completion = tr?.CompletionPercentage ?? 0,
                    audio_stream_index = ps?.AudioStreamIndex?.ToString(CultureInfo.InvariantCulture) ?? string.Empty,
                    subtitle_stream_index = ps?.SubtitleStreamIndex?.ToString(CultureInfo.InvariantCulture) ?? string.Empty,
                    media_source_id = ps?.MediaSourceId ?? string.Empty
                };

                var requestMessage = new HttpRequestMessage(HttpMethod.Post, reportUrl)
                {
                    Content = new StringContent(
                        JsonSerializer.Serialize(payload),
                        Encoding.UTF8,
                        MediaTypeNames.Application.Json)
                };

                if (!string.IsNullOrWhiteSpace(config.SharedSecret))
                {
                    requestMessage.Headers.TryAddWithoutValidation("X-JellyProxy-Secret", config.SharedSecret);
                }

                var response = await client.SendAsync(requestMessage).ConfigureAwait(false);
                if (!response.IsSuccessStatusCode)
                {
                    _logger.LogDebug("JellyProxy playback report returned status {StatusCode} for session {SessionId}", response.StatusCode, session.Id);
                }
            }
            catch (Exception ex)
            {
                _logger.LogDebug(ex, "Failed to send playback report to JellyProxy");
            }
        });
    }

    private void Unsubscribe()
    {
        _sessionManager.PlaybackStart -= OnPlaybackStart;
        _sessionManager.PlaybackProgress -= OnPlaybackProgress;
        _sessionManager.PlaybackStopped -= OnPlaybackStopped;
        _sessionManager.SessionEnded -= OnSessionEnded;
    }

    /// <inheritdoc />
    public void Dispose()
    {
        if (_disposed)
        {
            return;
        }

        _cts?.Cancel();
        _cts?.Dispose();
        Unsubscribe();
        _activeSessions.Clear();
        _disposed = true;
    }

    private class SessionStateTracker
    {
        public string ItemId { get; set; } = string.Empty;
        public string PlaySessionId { get; set; } = string.Empty;
        public bool IsPaused { get; set; }
        public long PositionTicks { get; set; }
        public DateTimeOffset LastReportTime { get; set; }
        public DateTimeOffset LastStateChangeTime { get; set; }
    }
}
