package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
)

// Config holds all crawler settings. Req-Time/Req-Hash are OPTIONAL: the API
// does not validate them (verified empirically), but they are sent if set.
type Config struct {
	APIURL                 string  `json:"api_url"`
	ReqTime                string  `json:"req_time"`
	ReqHash                string  `json:"req_hash"`
	AppVersion             string  `json:"app_version"`
	DeviceToken            string  `json:"device_token"`
	Language               string  `json:"language"`
	PageSize               int     `json:"page_size"`
	SleepMinSeconds        float64 `json:"sleep_min_seconds"`
	SleepMaxSeconds        float64 `json:"sleep_max_seconds"`
	CycleHours             float64 `json:"cycle_hours"`
	CrawlWindowEnabled     bool    `json:"crawl_window_enabled"`
	CrawlStartHour         int     `json:"crawl_start_hour"`
	CrawlEndHour           int     `json:"crawl_end_hour"`
	DBPath                 string  `json:"db_path"`
	LogDir                 string  `json:"log_dir"`
	WebHost                string  `json:"web_host"`
	WebPort                int     `json:"web_port"`
	WebToken               string  `json:"web_token"`
	ProxyURL               string  `json:"proxy_url"`
	ProxyUser              string  `json:"proxy_user"`
	ProxyPass              string  `json:"proxy_pass"`
	ProxyFallbackDirect    bool    `json:"proxy_fallback_direct"`
	RemovedGraceCycles     int     `json:"removed_grace_cycles"`
	TelegramEnabled        bool    `json:"telegram_enabled"`
	TelegramBotToken       string  `json:"telegram_bot_token"`
	TelegramChatID         string  `json:"telegram_chat_id"`
	TelegramHeartbeatMin   int     `json:"telegram_heartbeat_alert_minutes"`
	URLEnrichmentEnabled   bool    `json:"url_enrichment_enabled"`
	AutoUpdateEnabled      bool    `json:"auto_update_enabled"`
	AutoUpdateBranch       string  `json:"auto_update_branch"`
	AutoUpdateIntervalSecs int     `json:"auto_update_interval_seconds"`
	StateDir               string  `json:"state_dir"`
	DryRun                 bool    `json:"dry_run"`
	CategoriesRent         []int   `json:"categories_rent"`
	CategoriesSale         []int   `json:"categories_sale"`
}

const defaultConfigJSON = `{
  "api_url": "https://pro-v3.aqar.app/graphql",
  "req_time": "1786225763209",
  "req_hash": "3b03587a9a0e318585fd649176227343aec2e262",
  "app_version": "3.69",
  "device_token": "42C3F61D-CFA7-4253-BF9E-65380F4ABBE9",
  "language": "ar",
  "page_size": 500,
  "sleep_min_seconds": 10.5,
  "sleep_max_seconds": 14.5,
  "cycle_hours": 24,
  "crawl_window_enabled": false,
  "crawl_start_hour": 2,
  "crawl_end_hour": 8,
  "db_path": "AQAR.db",
  "log_dir": "logs",
  "web_host": "0.0.0.0",
  "web_port": 9090,
  "web_token": "",
  "proxy_url": "",
  "proxy_user": "",
  "proxy_pass": "",
  "proxy_fallback_direct": true,
  "removed_grace_cycles": 2,
  "telegram_enabled": false,
  "telegram_bot_token": "",
  "telegram_chat_id": "",
  "telegram_heartbeat_alert_minutes": 30,
  "url_enrichment_enabled": true,
  "auto_update_enabled": true,
  "auto_update_branch": "main",
  "auto_update_interval_seconds": 120,
  "state_dir": "state",
  "dry_run": false,
  "categories_rent": [1,5,4,13,11,8,16,15,19,14,18,17,23,27,29,31,33,35,37,39,41,44,46,49,51,53,55,56,58],
  "categories_sale": [3,2,6,7,9,10,12,20,22,24,25,26,28,30,32,34,36,38,40,42,43,45,47,48,50,52,54,57]
}
`

// DefaultConfig returns the built-in defaults (used when no config file exists).
func DefaultConfig() Config {
	var c Config
	if err := json.Unmarshal([]byte(defaultConfigJSON), &c); err != nil {
		panic(err)
	}
	return c
}

// LoadConfig reads the JSON config file. If it does not exist, a default file
// is written next to it so the user can adjust settings (interval, page size,
// categories, optional Req-Time/Req-Hash pair).
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if werr := os.WriteFile(path, []byte(defaultConfigJSON+"\n"), 0o644); werr != nil {
				return cfg, fmt.Errorf("creating default config %s: %w", path, werr)
			}
			fmt.Printf("Created default config file: %s\n", path)
			return cfg, nil
		}
		return cfg, fmt.Errorf("reading config %s: %w", path, err)
	}
	if err := json.Unmarshal(loadConfigBytes(data), &cfg); err != nil {
		return cfg, fmt.Errorf("parsing config %s: %w", path, err)
	}
	return cfg, nil
}

// loadConfigBytes strips a UTF-8 BOM (common on Windows when the file is
// edited with PowerShell/Notepad) before JSON parsing.
func loadConfigBytes(data []byte) []byte {
	return bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
}

// ApplyFlags lets command-line flags override config-file values.
func (c *Config) ApplyFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.DBPath, "db", c.DBPath, "SQLite database file path")
	fs.Float64Var(&c.SleepMinSeconds, "sleep-min", c.SleepMinSeconds, "minimum seconds between page requests")
	fs.Float64Var(&c.SleepMaxSeconds, "sleep-max", c.SleepMaxSeconds, "maximum seconds between page requests")
	fs.IntVar(&c.PageSize, "page-size", c.PageSize, "listings per page request")
	fs.Float64Var(&c.CycleHours, "cycle-hours", c.CycleHours, "target duration of one full crawl cycle")
	fs.StringVar(&c.ReqTime, "req-time", c.ReqTime, "Req-Time header value (epoch millis)")
	fs.StringVar(&c.ReqHash, "req-hash", c.ReqHash, "Req-Hash header value (40 hex chars)")
	fs.IntVar(&c.WebPort, "web-port", c.WebPort, "control panel port")
}

func (c Config) validate() error {
	if c.APIURL == "" {
		return errors.New("api_url is empty")
	}
	if c.PageSize < 1 || c.PageSize > 1000 {
		return errors.New("page_size must be between 1 and 1000")
	}
	if c.SleepMinSeconds < 1 {
		return errors.New("sleep_min_seconds must be at least 1")
	}
	if c.SleepMaxSeconds <= c.SleepMinSeconds {
		return errors.New("sleep_max_seconds must be greater than sleep_min_seconds")
	}
	if c.CycleHours <= 0 {
		return errors.New("cycle_hours must be positive")
	}
	if c.CrawlStartHour < 0 || c.CrawlStartHour > 23 || c.CrawlEndHour < 0 || c.CrawlEndHour > 23 {
		return errors.New("crawl_start_hour/crawl_end_hour must be 0-23")
	}
	if c.CrawlStartHour == c.CrawlEndHour {
		return errors.New("crawl_start_hour and crawl_end_hour must differ")
	}
	if c.WebPort < 1 || c.WebPort > 65535 {
		return errors.New("web_port must be between 1 and 65535")
	}
	if c.TelegramEnabled {
		if c.TelegramBotToken == "" || c.TelegramChatID == "" {
			return errors.New("telegram_enabled requires telegram_bot_token and telegram_chat_id")
		}
	}
	if c.TelegramHeartbeatMin < 0 {
		return errors.New("telegram_heartbeat_alert_minutes must be >= 0 (0 = disabled)")
	}
	if len(c.CategoriesRent)+len(c.CategoriesSale) == 0 {
		return errors.New("no categories configured")
	}
	return nil
}
