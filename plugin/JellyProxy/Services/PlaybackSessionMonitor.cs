using System;
using System.Collections.Concurrent;
using System.Net.Http;
using System.Net.Mime;
using System.Text;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;
using JellyProxy.Configuration;
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
    private readonly IHttpClientFactory _httpClientFactory;
    private readonly ILogger<PlaybackSessionMonitor> _logger;
    private readonly ConcurrentDictionary<string, SessionStateTracker> _activeSessions = new();
    private bool _disposed;

    /// <summary>
    /// Initializes a new instance of the <see cref="PlaybackSessionMonitor"/> class.
    /// </summary>
    public PlaybackSessionMonitor(
        ISessionManager sessionManager,
        IHttpClientFactory httpClientFactory,
        ILogger<PlaybackSessionMonitor> logger)
    {
        _sessionManager = sessionManager;
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
        _logger.LogInformation("PlaybackSessionMonitor started and subscribed to session events.");
        return Task.CompletedTask;
    }

    /// <inheritdoc />
    public Task StopAsync(CancellationToken cancellationToken)
    {
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

        SendPlaybackReport(e.Session, e.Item?.Name, itemId, e.PlaySessionId, posTicks, e.Item?.RunTimeTicks, false, "start", e.Session.PlayState?.PlayMethod.ToString());
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

        // Heuristic Seek Detection: drift between position ticks delta and elapsed wall-clock time
        long elapsedTicks = (long)(now - state.LastReportTime).TotalMilliseconds * 10000;
        long tickDelta = Math.Abs(currentTicks - state.PositionTicks);
        bool isSeek = !isPaused && !state.IsPaused && tickDelta > (elapsedTicks + 50_000_000); // > 5s drift

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
        else if (now - state.LastReportTime >= TimeSpan.FromSeconds(20))
        {
            // Heartbeat report every 20 seconds
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

            SendPlaybackReport(e.Session, e.Item?.Name, itemId, e.PlaySessionId, currentTicks, e.Item?.RunTimeTicks, isPaused, eventName, e.Session.PlayState?.PlayMethod.ToString());
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

        SendPlaybackReport(e.Session, e.Item?.Name, itemId, e.PlaySessionId, posTicks, e.Item?.RunTimeTicks, false, "stop", e.Session.PlayState?.PlayMethod.ToString());
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
                    play_method = playMethod ?? "DirectPlay"
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
