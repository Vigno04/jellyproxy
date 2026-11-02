using System;
using MediaBrowser.Model.Plugins;

namespace JellyProxy.Configuration;

/// <summary>
/// Configuration options for the JellyProxy companion plugin.
/// </summary>
public class PluginConfiguration : BasePluginConfiguration
{
    /// <summary>
    /// Gets or sets the base URL where the JellyProxy container is running.
    /// E.g. "http://vps-ip:8080" or "https://jellyproxy.example.com".
    /// </summary>
    public string ProxyUrl { get; set; } = "http://localhost:8080";

    /// <summary>
    /// Gets or sets the shared secret authentication token for proxy pairing.
    /// </summary>
    public string SharedSecret { get; set; } = string.Empty;

    /// <summary>
    /// Gets or sets a value indicating whether background playback events are monitored.
    /// </summary>
    public bool AutoSyncPlayback { get; set; } = true;

    /// <summary>
    /// Gets or sets a value indicating whether intelligent bandwidth shaping is requested.
    /// </summary>
    public bool EnableBandwidthShaping { get; set; } = true;

    /// <summary>
    /// Gets or sets the timestamp of the last successful handshake with the proxy.
    /// </summary>
    public DateTime? LastSuccessfulHandshake { get; set; }

    /// <summary>
    /// Gets or sets status text returned by the last handshake probe.
    /// </summary>
    public string LastHandshakeStatus { get; set; } = "Not connected";
}
