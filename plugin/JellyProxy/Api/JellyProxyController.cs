using System;
using System.Linq;
using System.Net.Http;
using System.Net.Mime;
using System.Text;
using System.Text.Json;
using System.Threading.Tasks;
using JellyProxy.Configuration;
using JellyProxy.Services;
using MediaBrowser.Controller.Configuration;
using MediaBrowser.Controller.Entities;
using MediaBrowser.Controller.Library;
using Microsoft.AspNetCore.Http;
using Microsoft.AspNetCore.Mvc;
using Microsoft.Extensions.Logging;

namespace JellyProxy.Api;

/// <summary>
/// API Controller providing proxy testing, status, and pairing endpoints.
/// </summary>
[ApiController]
[Route("JellyProxy")]
[Produces(MediaTypeNames.Application.Json)]
public class JellyProxyController : ControllerBase
{
    private readonly IHttpClientFactory _httpClientFactory;
    private readonly IServerConfigurationManager _serverConfigurationManager;
    private readonly ILibraryManager _libraryManager;
    private readonly ILogger<JellyProxyController> _logger;

    /// <summary>
    /// Initializes a new instance of the <see cref="JellyProxyController"/> class.
    /// </summary>
    public JellyProxyController(
        IHttpClientFactory httpClientFactory,
        IServerConfigurationManager serverConfigurationManager,
        ILibraryManager libraryManager,
        ILogger<JellyProxyController> logger)
    {
        _httpClientFactory = httpClientFactory;
        _serverConfigurationManager = serverConfigurationManager;
        _libraryManager = libraryManager;
        _logger = logger;
    }

    /// <summary>
    /// Test connection and perform handshake with remote JellyProxy instance.
    /// </summary>
    [HttpPost("TestConnection")]
    [ProducesResponseType(StatusCodes.Status200OK)]
    [ProducesResponseType(StatusCodes.Status400BadRequest)]
    public async Task<ActionResult<object>> TestConnection([FromBody] TestConnectionRequest? request)
    {
        var config = Plugin.Instance?.Configuration ?? new PluginConfiguration();
        var proxyUrl = !string.IsNullOrWhiteSpace(request?.ProxyUrl) ? request.ProxyUrl : config.ProxyUrl;
        var secret = request?.SharedSecret ?? config.SharedSecret;

        if (string.IsNullOrWhiteSpace(proxyUrl))
        {
            return BadRequest(new { success = false, message = "Proxy URL is required." });
        }

        proxyUrl = proxyUrl.TrimEnd('/');
        var handshakeUrl = $"{proxyUrl}/jellyproxy/api/handshake";

        try
        {
            var client = _httpClientFactory.CreateClient("JellyProxy");
            client.Timeout = TimeSpan.FromSeconds(10);

            var encoders = EncoderDetector.ResolveEncoders(_serverConfigurationManager, config, out var hwAccel);

            var payload = new
            {
                secret = secret,
                server_name = _serverConfigurationManager.Configuration.ServerName,
                server_id = _serverConfigurationManager.Configuration.ServerName,
                plugin_version = "0.6.0",
                jellyfin_version = "10.11.x",
                hw_accel = hwAccel,
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

            var requestMessage = new HttpRequestMessage(HttpMethod.Post, handshakeUrl)
            {
                Content = new StringContent(
                    JsonSerializer.Serialize(payload),
                    Encoding.UTF8,
                    MediaTypeNames.Application.Json)
            };

            if (!string.IsNullOrWhiteSpace(secret))
            {
                requestMessage.Headers.TryAddWithoutValidation("X-JellyProxy-Secret", secret);
            }

            var response = await client.SendAsync(requestMessage).ConfigureAwait(false);
            var responseBody = await response.Content.ReadAsStringAsync().ConfigureAwait(false);

            if (response.IsSuccessStatusCode)
            {
                if (Plugin.Instance != null)
                {
                    Plugin.Instance.Configuration.LastSuccessfulHandshake = DateTime.UtcNow;
                    Plugin.Instance.Configuration.LastHandshakeStatus = "Paired and verified";
                    Plugin.Instance.SaveConfiguration();
                }

                _logger.LogInformation("JellyProxy connection verified successfully with {Url}", proxyUrl);
                return Ok(new
                {
                    success = true,
                    message = "Successfully connected and paired with JellyProxy!",
                    details = responseBody
                });
            }

            var errorMsg = $"Proxy returned HTTP {response.StatusCode}: {responseBody}";
            _logger.LogWarning("JellyProxy connection test failed: {Error}", errorMsg);

            if (Plugin.Instance != null)
            {
                Plugin.Instance.Configuration.LastHandshakeStatus = errorMsg;
                Plugin.Instance.SaveConfiguration();
            }

            return Ok(new
            {
                success = false,
                message = errorMsg
            });
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Failed to connect to JellyProxy at {Url}", handshakeUrl);
            var errorMsg = $"Network error connecting to proxy: {ex.Message}";

            if (Plugin.Instance != null)
            {
                Plugin.Instance.Configuration.LastHandshakeStatus = errorMsg;
                Plugin.Instance.SaveConfiguration();
            }

            return Ok(new
            {
                success = false,
                message = errorMsg
            });
        }
    }

    /// <summary>
    /// Gets current pairing status and configuration summary.
    /// </summary>
    [HttpGet("Status")]
    public ActionResult<object> GetStatus()
    {
        var config = Plugin.Instance?.Configuration ?? new PluginConfiguration();
        return Ok(new
        {
            proxy_url = config.ProxyUrl,
            has_secret = !string.IsNullOrEmpty(config.SharedSecret),
            auto_sync_playback = config.AutoSyncPlayback,
            enable_shaping = config.EnableBandwidthShaping,
            max_storage_gb = config.MaxStorageGb,
            last_handshake = config.LastSuccessfulHandshake,
            last_status = config.LastHandshakeStatus
        });
    }

    /// <summary>
    /// Relays ABR status, config, and encoder capability requests to JellyProxy.
    /// </summary>
    [HttpGet("Abr/Status")]
    public async Task<ActionResult> GetAbrStatus() => await RelayAbr(HttpMethod.Get, "status", null);

    [HttpGet("Abr/Config")]
    public async Task<ActionResult> GetAbrConfig() => await RelayAbr(HttpMethod.Get, "config", null);

    [HttpPost("Abr/Config")]
    public async Task<ActionResult> UpdateAbrConfig([FromBody] object request) => await RelayAbr(HttpMethod.Post, "config", request);

    [HttpGet("Abr/Capabilities")]
    public async Task<ActionResult> GetAbrCapabilities() => await RelayAbr(HttpMethod.Get, "capabilities", null);

    [HttpGet("DetectedEncoders")]
    public ActionResult<object> GetDetectedEncoders()
    {
        var config = Plugin.Instance?.Configuration ?? new PluginConfiguration();
        var encoders = EncoderDetector.ResolveEncoders(_serverConfigurationManager, config, out var hwAccel);
        return Ok(new { encoders, hw_accel = hwAccel });
    }

    [HttpPost("Abr/Capabilities")]
    public async Task<ActionResult> UpdateAbrCapabilities([FromBody] object request) => await RelayAbr(HttpMethod.Post, "capabilities", request);

    private async Task<ActionResult> RelayAbr(HttpMethod method, string operation, object? payload)
    {
        var config = Plugin.Instance?.Configuration ?? new PluginConfiguration();
        if (string.IsNullOrWhiteSpace(config.ProxyUrl)) return BadRequest(new { message = "Proxy URL is not configured." });
        var message = new HttpRequestMessage(method, $"{config.ProxyUrl.TrimEnd('/')}/jellyproxy/api/abr/{operation}");
        if (payload != null) message.Content = new StringContent(JsonSerializer.Serialize(payload), Encoding.UTF8, MediaTypeNames.Application.Json);
        if (!string.IsNullOrWhiteSpace(config.SharedSecret)) message.Headers.TryAddWithoutValidation("X-JellyProxy-Secret", config.SharedSecret);
        try
        {
            using var cts = new System.Threading.CancellationTokenSource(TimeSpan.FromSeconds(5));
            var response = await _httpClientFactory.CreateClient("JellyProxy").SendAsync(message, cts.Token).ConfigureAwait(false);
            return new ContentResult { StatusCode = (int)response.StatusCode, ContentType = MediaTypeNames.Application.Json, Content = await response.Content.ReadAsStringAsync().ConfigureAwait(false) };
        }
        catch (Exception ex)
        {
            _logger.LogWarning(ex, "Failed to relay ABR request {Operation} to proxy", operation);
            return StatusCode(StatusCodes.Status502BadGateway, new { success = false, message = "Proxy unreachable: " + ex.Message });
        }
    }

    [HttpGet("Storage")]
    [ProducesResponseType(StatusCodes.Status200OK)]
    public async Task<ActionResult<object>> GetStorage()
    {
        var config = Plugin.Instance?.Configuration ?? new PluginConfiguration();
        if (string.IsNullOrWhiteSpace(config.ProxyUrl))
        {
            return BadRequest(new { success = false, message = "Proxy URL is not configured." });
        }

        var proxyUrl = config.ProxyUrl.TrimEnd('/');
        var storageUrl = $"{proxyUrl}/jellyproxy/api/storage";

        try
        {
            var client = _httpClientFactory.CreateClient("JellyProxy");
            client.Timeout = TimeSpan.FromSeconds(10);

            var requestMessage = new HttpRequestMessage(HttpMethod.Get, storageUrl);
            if (!string.IsNullOrWhiteSpace(config.SharedSecret))
            {
                requestMessage.Headers.TryAddWithoutValidation("X-JellyProxy-Secret", config.SharedSecret);
            }

            var response = await client.SendAsync(requestMessage).ConfigureAwait(false);
            var responseBody = await response.Content.ReadAsStringAsync().ConfigureAwait(false);

            if (response.IsSuccessStatusCode)
            {
                return Content(responseBody, MediaTypeNames.Application.Json);
            }

            return StatusCode((int)response.StatusCode, new { success = false, message = $"Proxy returned HTTP {response.StatusCode}: {responseBody}" });
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Failed to fetch storage stats from {Url}", storageUrl);
            return StatusCode(StatusCodes.Status500InternalServerError, new { success = false, message = $"Failed to fetch storage stats: {ex.Message}" });
        }
    }

    /// <summary>
    /// Purges cache categories on JellyProxy.
    /// </summary>
    [HttpPost("Purge")]
    [ProducesResponseType(StatusCodes.Status200OK)]
    public async Task<ActionResult<object>> PurgeCache([FromBody] PurgeRequest? request)
    {
        var config = Plugin.Instance?.Configuration ?? new PluginConfiguration();
        if (string.IsNullOrWhiteSpace(config.ProxyUrl))
        {
            return BadRequest(new { success = false, message = "Proxy URL is not configured." });
        }

        var proxyUrl = config.ProxyUrl.TrimEnd('/');
        var purgeUrl = $"{proxyUrl}/jellyproxy/api/cache/purge";

        try
        {
            var client = _httpClientFactory.CreateClient("JellyProxy");
            client.Timeout = TimeSpan.FromSeconds(30);

            var payload = new
            {
                secret = config.SharedSecret,
                category = request?.Category ?? "all"
            };

            var requestMessage = new HttpRequestMessage(HttpMethod.Post, purgeUrl)
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
            var responseBody = await response.Content.ReadAsStringAsync().ConfigureAwait(false);

            if (response.IsSuccessStatusCode)
            {
                return Content(responseBody, MediaTypeNames.Application.Json);
            }

            return StatusCode((int)response.StatusCode, new { success = false, message = $"Proxy returned HTTP {response.StatusCode}: {responseBody}" });
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Failed to purge cache at {Url}", purgeUrl);
            return StatusCode(StatusCodes.Status500InternalServerError, new { success = false, message = $"Failed to purge cache: {ex.Message}" });
        }
    }

    /// <summary>
    /// Updates storage configuration on JellyProxy and locally.
    /// </summary>
    [HttpPost("UpdateStorageConfig")]
    [ProducesResponseType(StatusCodes.Status200OK)]
    public async Task<ActionResult<object>> UpdateStorageConfig([FromBody] UpdateStorageConfigRequest? request)
    {
        if (request == null)
        {
            return BadRequest(new { success = false, message = "Request body is required." });
        }

        var config = Plugin.Instance?.Configuration ?? new PluginConfiguration();
        
        // Update local plugin config
        config.MaxStorageGb = request.MaxStorageGb;
        if (Plugin.Instance != null)
        {
            Plugin.Instance.SaveConfiguration();
        }

        if (string.IsNullOrWhiteSpace(config.ProxyUrl))
        {
            return Ok(new { success = true, message = "Local configuration updated, but Proxy URL is not configured." });
        }

        var proxyUrl = config.ProxyUrl.TrimEnd('/');
        var configUrl = $"{proxyUrl}/jellyproxy/api/config";

        try
        {
            var client = _httpClientFactory.CreateClient("JellyProxy");
            client.Timeout = TimeSpan.FromSeconds(10);

            var payload = new
            {
                secret = config.SharedSecret,
                max_storage_gb = request.MaxStorageGb
            };

            var requestMessage = new HttpRequestMessage(HttpMethod.Post, configUrl)
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
            var responseBody = await response.Content.ReadAsStringAsync().ConfigureAwait(false);

            if (response.IsSuccessStatusCode)
            {
                return Content(responseBody, MediaTypeNames.Application.Json);
            }

            return StatusCode((int)response.StatusCode, new { success = false, message = "Proxy storage config update returned HTTP " + response.StatusCode, details = responseBody });
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Failed to update storage config on proxy at {Url}", configUrl);
            return StatusCode(StatusCodes.Status500InternalServerError, new { success = false, message = "Failed to update storage config on proxy: " + ex.Message });
        }
    }

    [HttpPost("Preseed/Start")]
    public async Task<ActionResult> StartPreseed([FromBody] PreseedStartRequest request)
    {
        var config = Plugin.Instance?.Configuration ?? new PluginConfiguration();
        if (string.IsNullOrWhiteSpace(config.ProxyUrl)) return BadRequest(new { message = "Proxy URL is not configured." });
        var itemName = request.ItemName;
        if (string.IsNullOrWhiteSpace(itemName) && Guid.TryParse(request.ItemId, out var itemId)) itemName = _libraryManager.GetItemById(itemId)?.Name;
        var urls = !string.IsNullOrWhiteSpace(request.StreamUrl)
            ? new[] { request.StreamUrl }
            : (!string.IsNullOrWhiteSpace(request.ItemId) ? new[] { $"/Videos/{request.ItemId}/main.m3u8" } : (request.Urls ?? Array.Empty<string>()));
        var payload = new { id = request.Id, item_name = itemName, urls = urls, headers = request.Headers };
        return await RelayPreseed(HttpMethod.Post, "start", payload, config);
    }

    [HttpGet("Preseed/Jobs")]
    public async Task<ActionResult> GetPreseedJobs() => await RelayPreseed(HttpMethod.Get, "jobs", null, Plugin.Instance?.Configuration ?? new PluginConfiguration());

    [HttpPost("Preseed/Cancel")]
    public async Task<ActionResult> CancelPreseed([FromBody] PreseedCancelRequest request) => await RelayPreseed(HttpMethod.Post, "cancel", request, Plugin.Instance?.Configuration ?? new PluginConfiguration());

    private async Task<ActionResult> RelayPreseed(HttpMethod method, string operation, object? payload, PluginConfiguration config)
    {
        if (string.IsNullOrWhiteSpace(config.ProxyUrl)) return BadRequest(new { message = "Proxy URL is not configured." });
        var message = new HttpRequestMessage(method, $"{config.ProxyUrl.TrimEnd('/')}/jellyproxy/api/preseed/{operation}");
        if (payload != null) message.Content = new StringContent(JsonSerializer.Serialize(payload), Encoding.UTF8, MediaTypeNames.Application.Json);
        if (!string.IsNullOrWhiteSpace(config.SharedSecret)) message.Headers.TryAddWithoutValidation("X-JellyProxy-Secret", config.SharedSecret);
        try
        {
            using var cts = new System.Threading.CancellationTokenSource(TimeSpan.FromSeconds(5));
            var response = await _httpClientFactory.CreateClient("JellyProxy").SendAsync(message, cts.Token).ConfigureAwait(false);
            var body = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
            return new ContentResult { StatusCode = (int)response.StatusCode, ContentType = MediaTypeNames.Application.Json, Content = body };
        }
        catch (Exception ex)
        {
            _logger.LogWarning(ex, "Failed to relay Preseed request {Operation} to proxy", operation);
            return StatusCode(StatusCodes.Status502BadGateway, new { success = false, message = "Proxy unreachable: " + ex.Message });
        }
    }
}

public class PreseedStartRequest { public string? Id { get; set; } public string? ItemId { get; set; } public string? ItemName { get; set; } public string? StreamUrl { get; set; } public string[]? Urls { get; set; } public System.Collections.Generic.Dictionary<string, string[]>? Headers { get; set; } }
public class PreseedCancelRequest { public string? Id { get; set; } }

/// <summary>
/// Request DTO for testing proxy connection.
/// </summary>
public class TestConnectionRequest
{
    /// <summary>
    /// Gets or sets the target proxy URL.
    /// </summary>
    public string? ProxyUrl { get; set; }

    /// <summary>
    /// Gets or sets the shared secret token.
    /// </summary>
    public string? SharedSecret { get; set; }
}

/// <summary>
/// Request DTO for cache purge.
/// </summary>
public class PurgeRequest
{
    /// <summary>
    /// Gets or sets the category to purge (all, images, static, media).
    /// </summary>
    public string? Category { get; set; }
}

/// <summary>
/// Request DTO for updating storage config.
/// </summary>
public class UpdateStorageConfigRequest
{
    /// <summary>
    /// Gets or sets the maximum storage quota in GB.
    /// </summary>
    public double MaxStorageGb { get; set; }
}
