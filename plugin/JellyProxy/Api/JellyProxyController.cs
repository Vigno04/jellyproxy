using System;
using System.Net.Http;
using System.Net.Mime;
using System.Text;
using System.Text.Json;
using System.Threading.Tasks;
using JellyProxy.Configuration;
using MediaBrowser.Controller.Configuration;
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
    private readonly ILogger<JellyProxyController> _logger;

    /// <summary>
    /// Initializes a new instance of the <see cref="JellyProxyController"/> class.
    /// </summary>
    public JellyProxyController(
        IHttpClientFactory httpClientFactory,
        IServerConfigurationManager serverConfigurationManager,
        ILogger<JellyProxyController> logger)
    {
        _httpClientFactory = httpClientFactory;
        _serverConfigurationManager = serverConfigurationManager;
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

            var payload = new
            {
                secret = secret,
                server_name = _serverConfigurationManager.Configuration.ServerName,
                server_id = _serverConfigurationManager.Configuration.ServerName, // Or system ID
                plugin_version = "0.1.0",
                jellyfin_version = "10.10.x"
            };

            var content = new StringContent(
                JsonSerializer.Serialize(payload),
                Encoding.UTF8,
                MediaTypeNames.Application.Json);

            if (!string.IsNullOrWhiteSpace(secret))
            {
                content.Headers.Add("X-JellyProxy-Secret", secret);
            }

            var response = await client.PostAsync(handshakeUrl, content).ConfigureAwait(false);
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
            last_handshake = config.LastSuccessfulHandshake,
            last_status = config.LastHandshakeStatus
        });
    }
}

/// <summary>
/// Request DTO for test connection payload.
/// </summary>
public class TestConnectionRequest
{
    /// <summary>
    /// Target proxy URL.
    /// </summary>
    public string? ProxyUrl { get; set; }

    /// <summary>
    /// Shared secret token.
    /// </summary>
    public string? SharedSecret { get; set; }
}
