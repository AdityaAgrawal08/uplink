package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

var configKeys = map[string]string{
	"server":          "string",
	"expiry":          "string",
	"download_dir":    "string",
	"lan_port":        "int",
	"adaptive_chunks": "bool",
	"show_qr":         "string",
}

func handleConfig(args []string) {
	if len(args) < 1 {
		printConfigUsage()
		os.Exit(1)
	}
	switch args[0] {
	case "get":
		handleConfigGet(args[1:])
	case "set":
		handleConfigSet(args[1:])
	case "ls":
		handleConfigLs()
	default:
		fmt.Printf("✗ Unknown config subcommand %q\n\n", args[0])
		printConfigUsage()
		os.Exit(1)
	}
}

func printConfigUsage() {
	fmt.Println("Usage:")
	fmt.Println("  uplink config get <key>     Show a config value")
	fmt.Println("  uplink config set <key> <val>  Set a config value")
	fmt.Println("  uplink config ls            List all config values")
	fmt.Println()
	fmt.Println("Keys:")
	for k, t := range configKeys {
		fmt.Printf("  %-18s (%s)\n", k, t)
	}
}

func readConfigFile() map[string]any {
	path := getConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	return m
}

func writeConfigFile(m map[string]any) error {
	path := getConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func handleConfigGet(args []string) {
	if len(args) < 1 {
		fmt.Println("✗ Key required. Usage: uplink config get <key>")
		os.Exit(1)
	}
	key := args[0]
	if _, ok := configKeys[key]; !ok {
		fmt.Printf("✗ Unknown key %q. Run: uplink config ls\n", key)
		os.Exit(1)
	}
	cfg := LoadConfig()
	val := getConfigValue(cfg, key)
	fmt.Println(val)
}

func handleConfigSet(args []string) {
	if len(args) < 2 {
		fmt.Println("✗ Key and value required. Usage: uplink config set <key> <value>")
		os.Exit(1)
	}
	key, valStr := args[0], args[1]
	typ, ok := configKeys[key]
	if !ok {
		fmt.Printf("✗ Unknown key %q. Run: uplink config ls\n", key)
		os.Exit(1)
	}

	m := readConfigFile()
	if m == nil {
		m = make(map[string]any)
	}

	switch typ {
	case "int":
		v, err := strconv.Atoi(valStr)
		if err != nil {
			fmt.Printf("✗ Invalid integer: %v\n", err)
			os.Exit(1)
		}
		m[key] = v
	case "bool":
		v, err := strconv.ParseBool(valStr)
		if err != nil {
			fmt.Printf("✗ Invalid bool: %v\n", err)
			os.Exit(1)
		}
		m[key] = v
	default:
		if key == "server" {
			if strings.TrimSpace(valStr) == "" {
				fmt.Println("✗ server must not be empty")
				os.Exit(1)
			}
			valStr = sanitizeServerUrl(valStr)
		}
		m[key] = valStr
	}

	if err := writeConfigFile(m); err != nil {
		fmt.Printf("✗ Write failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ %s = %s\n", key, valStr)
}

func handleConfigLs() {
	cfg := LoadConfig()
	keys := make([]string, 0, len(configKeys))
	for k := range configKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-18s = %s\n", k, getConfigValue(cfg, k))
	}
}

func getConfigValue(cfg *Config, key string) string {
	switch key {
	case "server":
		return cfg.Server
	case "expiry":
		return cfg.Expiry
	case "download_dir":
		if cfg.DownloadDir == "" {
			return "(not set)"
		}
		return cfg.DownloadDir
	case "lan_port":
		return strconv.Itoa(cfg.LanPort)
	case "adaptive_chunks":
		return strconv.FormatBool(cfg.AdaptiveChunks)
	case "show_qr":
		return cfg.ShowQR
	default:
		return ""
	}
}
