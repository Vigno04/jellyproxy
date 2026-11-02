using System;
using System.Net.Http;
using System.Net.Mime;
using System.Text;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;
using JellyProxy.Configuration;
using MediaBrowser.Controller.Library;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;

namespace JellyProxy.Services;

/// <summary>
/// Background service that listens for library item updates and removals,
/// and notifies the JellyProxy cache invalidation endpoint.
/// </summary>
public class LibraryEventNotifier : IHostedService, IDisposable
{
    private readonly ILibraryManager _libraryManager;
    private readonly IHttpClientFactory _httpClientFactory;
    private readonly ILogger<LibraryEventNotifier> _logger;
    private bool _disposed;

    /// <summary>
    /// Initializes a new instance of the <see cref="LibraryEventNotifier"/> class.
    /// </summary>
    public LibraryEventNotifier(
        ILibraryManager libraryManager,
        IHttpClientFactory httpClientFactory,
        ILogger<LibraryEventNotifier> logger)
    {
        _libraryManager = libraryManager;
        _httpClientFactory = httpClientFactory;
        _logger = logger;
    }

    /// <inheritdoc />
    public Task StartAsync(CancellationToken cancellationToken)
    {
        _libraryManager.ItemUpdated += OnItemUpdated;
        _libraryManager.ItemRemoved += OnItemRemoved;
        _logger.LogInformation("LibraryEventNotifier started and subscribed to library events.");
        return Task.CompletedTask;
    }

    /// <inheritdoc />
    public Task StopAsync(CancellationToken cancellationToken)
    {
        Unsubscribe();
        _logger.LogInformation("LibraryEventNotifier stopped and unsubscribed from library events.");
        return Task.CompletedTask;
    }

    private void OnItemUpdated(object? sender, ItemChangeEventArgs e)
    {
        if (e?.Item == null)
        {
            return;
        }

        FireInvalidate(e.Item.Id.ToString("N"), "image");
    }

    private void OnItemRemoved(object? sender, ItemChangeEventArgs e)
    {
        if (e?.Item == null)
        {
            return;
        }

        FireInvalidate(e.Item.Id.ToString("N"), "image");
    }

    private void FireInvalidate(string itemId, string type)
    {
        _ = Task.Run(async () =>
        {
            try
            {
                var config = Plugin.Instance?.Configuration;
                if (config == null || string.IsNullOrWhiteSpace(config.ProxyUrl))
                {
                    return;
                }

                var proxyUrl = config.ProxyUrl.TrimEnd('/');
                var invalidateUrl = $"{proxyUrl}/jellyproxy/api/cache/invalidate";

                var client = _httpClientFactory.CreateClient("JellyProxy");
                client.Timeout = TimeSpan.FromSeconds(5);

                var payload = new
                {
                    secret = config.SharedSecret,
                    item_id = itemId,
                    type = type
                };

                var requestMessage = new HttpRequestMessage(HttpMethod.Post, invalidateUrl)
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
                    _logger.LogWarning("JellyProxy cache invalidate returned status {StatusCode} for item {ItemId}", response.StatusCode, itemId);
                }
            }
            catch (Exception ex)
            {
                _logger.LogDebug(ex, "Failed to notify JellyProxy cache invalidation for item {ItemId}", itemId);
            }
        });
    }

    private void Unsubscribe()
    {
        try
        {
            _libraryManager.ItemUpdated -= OnItemUpdated;
            _libraryManager.ItemRemoved -= OnItemRemoved;
        }
        catch (Exception ex)
        {
            _logger.LogDebug(ex, "Error unsubscribing from library manager events");
        }
    }

    /// <inheritdoc />
    public void Dispose()
    {
        if (!_disposed)
        {
            Unsubscribe();
            _disposed = true;
        }
        GC.SuppressFinalize(this);
    }
}