-- plugin_capabilities caches each registered plugin's parsed --init
-- document so server startup skips the --init shell-out when a plugin's
-- version (cheap to probe) hasn't changed since the last cache write.
--
-- plugin_name is the operator-set name from the config allowlist (per
-- ADR-0006); version is the plugin-emitted version string from --init
-- / --version. (plugin_name) alone is the primary key — only the most
-- recent caps for a plugin are kept; bumping version overwrites in
-- place. capabilities_json is the verbatim JSON document the plugin
-- emitted to stdout on --init, decoded back into plugins.Capabilities
-- by the store layer on read. cached_at is RFC3339 UTC.
CREATE TABLE plugin_capabilities (
    plugin_name        TEXT PRIMARY KEY,
    version            TEXT NOT NULL,
    capabilities_json  TEXT NOT NULL,
    cached_at          TEXT NOT NULL
);
