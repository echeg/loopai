package t3

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// LoopaiModel is the single model advertised by loopai-acp.
const LoopaiModel = "loopai"

// maxSettingsSize bounds the settings file read to 1 MiB.
const maxSettingsSize = 1 << 20

// ProviderInstance identifies an enabled loopai-acp instance in T3 Code settings.
type ProviderInstance struct {
	ID         string
	Driver     string
	Enabled    bool
	BinaryPath string
}

type settingsInstance struct {
	Driver  string `json:"driver"`
	Enabled *bool  `json:"enabled"`
	Config  struct {
		Enabled    *bool  `json:"enabled"`
		BinaryPath string `json:"binaryPath"`
	} `json:"config"`
}

// FindLoopaiInstance reads T3 Code settings without modifying them and returns the first usable
// loopai-acp instance in sorted key order. Missing settings or instances are not errors.
func FindLoopaiInstance(getenv func(string) string) (ProviderInstance, bool, error) {
	home, err := HomeDir(getenv)
	if err != nil {
		return ProviderInstance{}, false, err
	}
	f, err := os.Open(filepath.Join(home, "userdata", "settings.json")) //nolint:gosec // trusted T3 home from environment
	if errors.Is(err, os.ErrNotExist) {
		return ProviderInstance{}, false, nil
	}
	if err != nil {
		return ProviderInstance{}, false, fmt.Errorf("t3: open settings file: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSettingsSize+1))
	if err != nil {
		return ProviderInstance{}, false, fmt.Errorf("t3: read settings file: %w", err)
	}
	if len(data) > maxSettingsSize {
		return ProviderInstance{}, false, fmt.Errorf("t3: settings file exceeds %d bytes", maxSettingsSize)
	}
	var settings struct {
		ProviderInstances map[string]settingsInstance `json:"providerInstances"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return ProviderInstance{}, false, fmt.Errorf("t3: parse settings file: %w", err)
	}
	ids := make([]string, 0, len(settings.ProviderInstances))
	for id := range settings.ProviderInstances {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		instance := settings.ProviderInstances[id]
		if id == "" || instance.Driver != "grok" ||
			(instance.Enabled != nil && !*instance.Enabled) ||
			(instance.Config.Enabled != nil && !*instance.Config.Enabled) {
			continue
		}
		// Recognize both path separators, including Windows settings read on another platform.
		base := filepath.Base(strings.ReplaceAll(instance.Config.BinaryPath, `\`, "/"))
		if base != "loopai-acp" && base != "loopai-acp.exe" {
			continue
		}
		return ProviderInstance{
			ID: id, Driver: instance.Driver, Enabled: true, BinaryPath: instance.Config.BinaryPath,
		}, true, nil
	}
	return ProviderInstance{}, false, nil
}
