package main

import (
	"encoding/json"
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
		Server:         "https://uplink-delta-xi.vercel.app",
		Expiry:         "1h",
		DownloadDir:    "",
		LanPort:        9090,
		AdaptiveChunks: true,
		ShowQR:         "auto",
	}
}

func getConfigPath() string {
	home, _ := os.UserHomeDir()
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
		if json.Unmarshal(data, &fileCfg) == nil {
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
