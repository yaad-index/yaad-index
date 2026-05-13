//go:build e2e

package e2e

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// configFile is the test-facing input; writeConfig projects to
// configYAML on emit (the daemon-recognized disk shape).
type configFile struct {
	Plugins            []PluginConfig                `yaml:"-"`
	PluginBins         map[string]string             `yaml:"-"`
	VaultPath          string                        `yaml:"-"`
	CanonicalKinds     map[string]CanonicalKindEntry `yaml:"canonical_kinds,omitempty"`
	CanonicalEdgeTypes []string                      `yaml:"canonical_edge_types,omitempty"`
}

// configYAML is the on-disk shape matching internal/config.Config.
type configYAML struct {
	Plugins            []configPluginEntry           `yaml:"plugins"`
	Vault              configVaultEntry              `yaml:"vault"`
	CanonicalKinds     map[string]CanonicalKindEntry `yaml:"canonical_kinds,omitempty"`
	CanonicalEdgeTypes []string                      `yaml:"canonical_edge_types,omitempty"`
}

type configPluginEntry struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

type configVaultEntry struct {
	Path string `yaml:"path"`
}

// writeConfig marshals + writes the daemon's config.yaml at path.
func writeConfig(t *testing.T, path string, cfg configFile) {
	t.Helper()
	pluginEntries := make([]configPluginEntry, 0, len(cfg.Plugins))
	for _, p := range cfg.Plugins {
		pluginEntries = append(pluginEntries, configPluginEntry{
			Name: p.Name,
			Path: cfg.PluginBins[p.Name],
		})
	}
	out := configYAML{
		Plugins:            pluginEntries,
		Vault:              configVaultEntry{Path: cfg.VaultPath},
		CanonicalKinds:     cfg.CanonicalKinds,
		CanonicalEdgeTypes: cfg.CanonicalEdgeTypes,
	}
	body, err := yaml.Marshal(out)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, body, 0o644))
}
