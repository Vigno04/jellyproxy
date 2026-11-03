using System;
using System.Collections.Generic;
using System.Linq;
using JellyProxy.Configuration;
using MediaBrowser.Controller.Configuration;

namespace JellyProxy.Services;

/// <summary>
/// Inspects Jellyfin's transcoding configuration (encoding.xml) to determine which
/// video codecs the server has enabled for hardware/software encoding.
/// </summary>
public static class EncoderDetector
{
    public static string[] ResolveEncoders(IServerConfigurationManager? configManager, PluginConfiguration? pluginConfig, out string hwAccel)
    {
        hwAccel = "none";
        bool allowHevc = true;
        bool allowAv1 = false;

        try
        {
            var encodingObj = configManager?.GetConfiguration("encoding");
            if (encodingObj != null)
            {
                var type = encodingObj.GetType();
                var hwProp = type.GetProperty("HardwareAccelerationType");
                if (hwProp?.GetValue(encodingObj) is object hwVal)
                {
                    hwAccel = hwVal.ToString()?.ToLowerInvariant() ?? "none";
                }

                var hevcProp = type.GetProperty("AllowHevcEncoding");
                if (hevcProp?.GetValue(encodingObj) is bool hevcVal)
                {
                    allowHevc = hevcVal;
                }

                var av1Prop = type.GetProperty("AllowAv1Encoding");
                if (av1Prop?.GetValue(encodingObj) is bool av1Val)
                {
                    allowAv1 = av1Val;
                }
            }
        }
        catch
        {
            // Fall back to safe defaults if reflection fails
        }

        var serverAllowed = new HashSet<string>(StringComparer.OrdinalIgnoreCase) { "h264" };
        if (allowHevc)
        {
            serverAllowed.Add("hevc");
        }
        if (allowAv1)
        {
            serverAllowed.Add("av1");
        }

        var configured = (pluginConfig?.SupportedEncoders ?? string.Empty).Trim();
        if (string.IsNullOrEmpty(configured) || string.Equals(configured, "auto", StringComparison.OrdinalIgnoreCase))
        {
            return serverAllowed.ToArray();
        }

        var requested = configured
            .Split(',', StringSplitOptions.RemoveEmptyEntries | StringSplitOptions.TrimEntries)
            .Select(NormalizeCodec)
            .Where(c => !string.IsNullOrEmpty(c) && serverAllowed.Contains(c))
            .Distinct(StringComparer.OrdinalIgnoreCase)
            .ToArray();

        return requested.Length > 0 ? requested : new[] { "h264" };
    }

    private static string NormalizeCodec(string codec)
    {
        var c = codec.Trim().ToLowerInvariant();
        return c switch
        {
            "h265" or "x265" or "hevc" => "hevc",
            "h264" or "x264" or "avc" => "h264",
            "av1" or "av01" => "av1",
            "vp9" => "vp9",
            _ => c
        };
    }
}

