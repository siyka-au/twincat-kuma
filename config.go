package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

type WebhookEntry struct {
	UptimeKuma string `yaml:"uptime-kuma"`
	Post       string `yaml:"post"`
}

type ConnectionConfig struct {
	TargetNetID string
	RouterHost  string
	RouterPort  int
	Timeout     time.Duration
}

type ServerConfig struct {
	Listen string
}

type Config struct {
	Server       ServerConfig
	Connection   ConnectionConfig
	PollInterval time.Duration
	PushInterval time.Duration
	AdsWebhooks  []WebhookEntry
	PlcWebhooks  []WebhookEntry
}

type FlagOverrides struct {
	ConfigPath   string
	TargetNetID  string
	RouterHost   string
	RouterPort   int
	Timeout      time.Duration
	Listen       string
	PollInterval time.Duration
}

func defaultConfig() Config {
	return Config{
		Server: ServerConfig{Listen: ":8080"},
		Connection: ConnectionConfig{
			TargetNetID: "127.0.0.1.1.1",
			RouterHost:  "127.0.0.1",
			RouterPort:  48898,
			Timeout:     2 * time.Second,
		},
		PollInterval: 10 * time.Second,
		PushInterval: 30 * time.Second,
	}
}

// rawConfig mirrors Config but stores duration fields as strings for YAML parsing.
// yaml.v3 does not natively unmarshal time.Duration from strings like "2s".
type rawConfig struct {
	Server struct {
		Listen string `yaml:"listen"`
	} `yaml:"server"`
	Connection struct {
		TargetNetID string `yaml:"target_net_id"`
		RouterHost  string `yaml:"router_host"`
		RouterPort  int    `yaml:"router_port"`
		Timeout     string `yaml:"timeout"`
	} `yaml:"connection"`
	PollInterval string         `yaml:"poll_interval"`
	PushInterval string         `yaml:"push_interval"`
	AdsWebhooks  []WebhookEntry `yaml:"ads"`
	PlcWebhooks  []WebhookEntry `yaml:"plc"`
}

func loadYAML(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: reading %s: %w", path, err)
	}
	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("config: parsing %s: %w", path, err)
	}

	if raw.Server.Listen != "" {
		cfg.Server.Listen = raw.Server.Listen
	}
	if raw.Connection.TargetNetID != "" {
		cfg.Connection.TargetNetID = raw.Connection.TargetNetID
	}
	if raw.Connection.RouterHost != "" {
		cfg.Connection.RouterHost = raw.Connection.RouterHost
	}
	if raw.Connection.RouterPort != 0 {
		cfg.Connection.RouterPort = raw.Connection.RouterPort
	}
	if raw.Connection.Timeout != "" {
		d, err := time.ParseDuration(raw.Connection.Timeout)
		if err != nil {
			return fmt.Errorf("config: invalid connection.timeout %q: %w", raw.Connection.Timeout, err)
		}
		cfg.Connection.Timeout = d
	}
	if raw.PollInterval != "" {
		d, err := time.ParseDuration(raw.PollInterval)
		if err != nil {
			return fmt.Errorf("config: invalid poll_interval %q: %w", raw.PollInterval, err)
		}
		cfg.PollInterval = d
	}
	if raw.PushInterval != "" {
		d, err := time.ParseDuration(raw.PushInterval)
		if err != nil {
			return fmt.Errorf("config: invalid push_interval %q: %w", raw.PushInterval, err)
		}
		cfg.PushInterval = d
	}
	if len(raw.AdsWebhooks) > 0 {
		cfg.AdsWebhooks = raw.AdsWebhooks
	}
	if len(raw.PlcWebhooks) > 0 {
		cfg.PlcWebhooks = raw.PlcWebhooks
	}
	return nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("ADS_TARGET_NET_ID"); v != "" {
		cfg.Connection.TargetNetID = v
	}
	if v := os.Getenv("ADS_ROUTER_HOST"); v != "" {
		cfg.Connection.RouterHost = v
	}
	if v := os.Getenv("ADS_ROUTER_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Connection.RouterPort = p
		}
	}
	if v := os.Getenv("ADS_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Connection.Timeout = d
		}
	}
	if v := os.Getenv("SERVER_LISTEN"); v != "" {
		cfg.Server.Listen = v
	}
	if v := os.Getenv("POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.PollInterval = d
		}
	}
}

func applyFlags(cfg *Config, f FlagOverrides) {
	if f.TargetNetID != "" {
		cfg.Connection.TargetNetID = f.TargetNetID
	}
	if f.RouterHost != "" {
		cfg.Connection.RouterHost = f.RouterHost
	}
	if f.RouterPort != 0 {
		cfg.Connection.RouterPort = f.RouterPort
	}
	if f.Timeout != 0 {
		cfg.Connection.Timeout = f.Timeout
	}
	if f.Listen != "" {
		cfg.Server.Listen = f.Listen
	}
	if f.PollInterval != 0 {
		cfg.PollInterval = f.PollInterval
	}
}

// LoadConfig resolves the full configuration: defaults → YAML → ENV → flags.
func LoadConfig(f FlagOverrides) (Config, error) {
	cfg := defaultConfig()

	cfgPath := os.Getenv("CONFIG")
	if f.ConfigPath != "" {
		cfgPath = f.ConfigPath
	}
	if cfgPath != "" {
		if err := loadYAML(cfgPath, &cfg); err != nil {
			return cfg, err
		}
	}

	applyEnv(&cfg)
	applyFlags(&cfg, f)
	return cfg, nil
}
