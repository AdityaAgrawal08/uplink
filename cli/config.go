package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

type Config struct {
	Server         string `json:"server"`
	Expiry         string `json:"expiry"`
	DownloadDir    string `json:"download_dir"`
	LanPort        int    `json:"lan_port"`
	AdaptiveChunks bool   `json:"adaptive_chunks"`
	ShowQR         string `json:"show_qr"`
}

func defaultConfig() *Config {
	return &Config{
		// Self-hosted EC2 box (HTTP until a domain/TLS exists). The Vercel
		// deployment stays live as a fallback: `uplink config set server
		// https://uplink-delta-xi.vercel.app`.
		Server:         "http://52.7.217.135:3000",
		Expiry:         "1h",
		DownloadDir:    "",
		LanPort:        9090,
		AdaptiveChunks: true,
		ShowQR:         "auto",
	}
}

func getConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".", ".uplink", "config.json")
	}
	return filepath.Join(home, ".uplink", "config.json")
}

func LoadConfig() *Config {
	cfg := defaultConfig()

	// 1. Config file overrides defaults
	path := getConfigPath()
	if data, err := os.ReadFile(path); err == nil {
		type fileConfig struct {
			Server         *string `json:"server"`
			Expiry         *string `json:"expiry"`
			DownloadDir    *string `json:"download_dir"`
			LanPort        *int    `json:"lan_port"`
			AdaptiveChunks *bool   `json:"adaptive_chunks"`
			ShowQR         *string `json:"show_qr"`
		}
		var fileCfg fileConfig
		if err := json.Unmarshal(data, &fileCfg); err != nil {
			fmt.Fprintf(os.Stderr, "warning: ignoring malformed config %s: %v\n", path, err)
		} else {
			if fileCfg.Server != nil {
				cfg.Server = *fileCfg.Server
			}
			if fileCfg.Expiry != nil {
				cfg.Expiry = *fileCfg.Expiry
			}
			if fileCfg.DownloadDir != nil {
				cfg.DownloadDir = *fileCfg.DownloadDir
			}
			if fileCfg.LanPort != nil {
				cfg.LanPort = *fileCfg.LanPort
			}
			if fileCfg.AdaptiveChunks != nil {
				cfg.AdaptiveChunks = *fileCfg.AdaptiveChunks
			}
			if fileCfg.ShowQR != nil {
				cfg.ShowQR = *fileCfg.ShowQR
			}
		}
	}

	// 2. Env vars override config file (highest priority)
	if s := os.Getenv("UPLINK_SERVER"); s != "" {
		cfg.Server = s
	}
	if e := os.Getenv("UPLINK_EXPIRY"); e != "" {
		cfg.Expiry = e
	}
	if d := os.Getenv("UPLINK_DOWNLOAD_DIR"); d != "" {
		cfg.DownloadDir = d
	}
	if p := os.Getenv("UPLINK_LAN_PORT"); p != "" {
		if val, err := strconv.Atoi(p); err == nil {
			cfg.LanPort = val
		}
	}
	if a := os.Getenv("UPLINK_ADAPTIVE_CHUNKS"); a != "" {
		if val, err := strconv.ParseBool(a); err == nil {
			cfg.AdaptiveChunks = val
		}
	}
	if q := os.Getenv("UPLINK_SHOW_QR"); q != "" {
		cfg.ShowQR = q
	}

	return cfg
}
