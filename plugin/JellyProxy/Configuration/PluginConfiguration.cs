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
    public int UploadSpeedMbps { get; set; } = 16;
    public int HeadroomPercent { get; set; } = 10;
    public string ShaperPolicy { get; set; } = "FairShare";
    public bool AllowDegradingExisting { get; set; } = true;
    public int MinBitrateMbps { get; set; } = 2;
    public int SingleClientCeilingMbps { get; set; }
    public int FastStartSeconds { get; set; } = 10;
    public int FastStartSharePercent { get; set; } = 90;
    public int SeekBorrowSeconds { get; set; } = 4;
    public int BackgroundFloorPercent { get; set; } = 5;
    public string SupportedEncoders { get; set; } = "auto";

    /// <summary>
    /// Gets or sets the timestamp of the last successful handshake with the proxy.
    /// </summary>
    public DateTime? LastSuccessfulHandshake { get; set; }

    /// <summary>
    /// Gets or sets status text returned by the last handshake probe.
    /// </summary>
    public string LastHandshakeStatus { get; set; } = "Not connected";

    /// <summary>
    /// Gets or sets the maximum storage quota allocated for cache in GB.
    /// </summary>
    public double MaxStorageGb { get; set; } = 50.0;
}
