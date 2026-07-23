package packaging

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func roots(t *testing.T) (pluginRoot, repoRoot string) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve packaging test path")
	}
	pluginRoot = filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	repoRoot = filepath.Clean(filepath.Join(pluginRoot, "..", ".."))
	return pluginRoot, repoRoot
}

func readJSON(t *testing.T, path string, out any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

func TestMarketplacePolicyAndVersions(t *testing.T) {
	pluginRoot, repoRoot := roots(t)

	var marketplace struct {
		Metadata struct {
			Version string `json:"version"`
		} `json:"metadata"`
		Plugins []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
			Policy  struct {
				Installation   string `json:"installation"`
				Authentication string `json:"authentication"`
			} `json:"policy"`
		} `json:"plugins"`
	}
	readJSON(t, filepath.Join(repoRoot, ".agents", "plugins", "marketplace.json"), &marketplace)

	var marketplacePlugin *struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Policy  struct {
			Installation   string `json:"installation"`
			Authentication string `json:"authentication"`
		} `json:"policy"`
	}
	for i := range marketplace.Plugins {
		if marketplace.Plugins[i].Name == "scrcpymac-phone-agent" {
			marketplacePlugin = &marketplace.Plugins[i]
			break
		}
	}
	if marketplacePlugin == nil {
		t.Fatal("scrcpymac-phone-agent is missing from marketplace.json")
	}
	if marketplacePlugin.Policy.Installation != "AVAILABLE" {
		t.Errorf("installation policy = %q, want AVAILABLE", marketplacePlugin.Policy.Installation)
	}
	if marketplacePlugin.Policy.Authentication != "ON_INSTALL" {
		t.Errorf("authentication policy = %q, want ON_INSTALL", marketplacePlugin.Policy.Authentication)
	}

	type manifest struct {
		Version string `json:"version"`
	}
	var codex, cursor, ui manifest
	readJSON(t, filepath.Join(pluginRoot, ".codex-plugin", "plugin.json"), &codex)
	readJSON(t, filepath.Join(pluginRoot, ".cursor-plugin", "plugin.json"), &cursor)
	readJSON(t, filepath.Join(pluginRoot, "ui", "package.json"), &ui)

	versionSource, err := os.ReadFile(filepath.Join(pluginRoot, "go", "internal", "version", "version.go"))
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`var Version = "([^"]+)"`).FindSubmatch(versionSource)
	if len(match) != 2 {
		t.Fatal("go/internal/version/version.go does not declare Version")
	}

	versions := []string{
		marketplace.Metadata.Version,
		marketplacePlugin.Version,
		codex.Version,
		cursor.Version,
		ui.Version,
		string(match[1]),
	}
	for _, got := range versions[1:] {
		if got != versions[0] {
			t.Fatalf("package versions disagree: %v", versions)
		}
	}
}

func TestGoOnlyRuntimeLayout(t *testing.T) {
	pluginRoot, _ := roots(t)

	for _, removed := range []string{
		"server",
		".venv",
		filepath.Join("scripts", "ensure-runtime.sh"),
		filepath.Join("go", "scripts", "parity"),
	} {
		if _, err := os.Stat(filepath.Join(pluginRoot, removed)); !os.IsNotExist(err) {
			t.Errorf("obsolete Python runtime path still exists: %s", removed)
		}
	}

	err := filepath.WalkDir(pluginRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == "node_modules" || entry.Name() == "dist") {
			return filepath.SkipDir
		}
		switch {
		case strings.HasSuffix(entry.Name(), ".py"):
			t.Errorf("Python source still exists: %s", path)
		case entry.Name() == "pyproject.toml", entry.Name() == "uv.lock":
			t.Errorf("Python package metadata still exists: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEntrypointsAndAssetsArePackaged(t *testing.T) {
	pluginRoot, _ := roots(t)

	var privateConfig, publicConfig map[string]any
	readJSON(t, filepath.Join(pluginRoot, ".mcp.json"), &privateConfig)
	readJSON(t, filepath.Join(pluginRoot, "mcp.json"), &publicConfig)
	privateBytes, _ := json.Marshal(privateConfig)
	publicBytes, _ := json.Marshal(publicConfig)
	if string(privateBytes) != string(publicBytes) {
		t.Fatal(".mcp.json and mcp.json disagree")
	}

	server := privateConfig["mcpServers"].(map[string]any)["scrcpymac-phone-agent"].(map[string]any)
	if server["command"] != "./mcp-server.sh" || server["cwd"] != "." {
		t.Fatalf("MCP entrypoint is not plugin-relative: %#v", server)
	}

	for _, path := range []string{
		"mcp-server.sh",
		filepath.Join("bin", "phone-agent"),
		filepath.Join("bin", "darwin", "arm64", "phone-agent"),
		filepath.Join("bin", "darwin", "x86_64", "phone-agent"),
		filepath.Join("bin", "darwin", "adb"),
	} {
		info, err := os.Stat(filepath.Join(pluginRoot, path))
		if err != nil {
			t.Errorf("missing packaged executable %s: %v", path, err)
			continue
		}
		if info.Mode()&0o111 == 0 {
			t.Errorf("%s is not executable", path)
		}
	}

	for path, minimum := range map[string]int64{
		filepath.Join("share", "scrcpy-server"):                                   80_000,
		filepath.Join("bin", "darwin", "adb"):                                     10_000_000,
		filepath.Join("go", "internal", "widget", "assets", "scrcpymac-app.html"): 100_000,
	} {
		info, err := os.Stat(filepath.Join(pluginRoot, path))
		if err != nil {
			t.Errorf("missing packaged asset %s: %v", path, err)
			continue
		}
		if info.Size() <= minimum {
			t.Errorf("%s is %d bytes, want more than %d", path, info.Size(), minimum)
		}
	}
}
