// Package config loads and validates the website-stats YAML configuration.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so it can be expressed in YAML as "10s", "5m", ...
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = v
	return nil
}

// ParseDurationOr parses a duration string ("10s", "5m") or returns def when
// the string is empty; non-empty invalid strings yield an error.
func ParseDurationOr(s string, def time.Duration) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	return time.ParseDuration(s)
}

func (d Duration) MarshalYAML() (interface{}, error) { return d.String(), nil }

type Config struct {
	Listen       string   `yaml:"listen"`
	DataDir      string   `yaml:"data_dir"`
	PasswordHash string   `yaml:"password_hash"`
	SessionTTL   Duration `yaml:"session_ttl"`

	Collect   CollectConfig   `yaml:"collect"`
	Retention RetentionConfig `yaml:"retention"`
	Alerts    AlertConfig     `yaml:"alerts"`
}

type CollectConfig struct {
	Interval      Duration `yaml:"interval"`        // /proc scan period
	NginxSitesDir string   `yaml:"nginx_sites_dir"` // vhost directory
	FpmPoolGlob   string   `yaml:"fpm_pool_glob"`   // glob for PHP-FPM pool files
	FpmStatusPath string   `yaml:"fpm_status_path"` // nginx location serving pool status
	FpmInterval   Duration `yaml:"fpm_interval"`
	DiskInterval  Duration `yaml:"disk_interval"`
	DiscoverEvery Duration `yaml:"discover_every"` // re-scan vhosts/pools
	TopWindow     Duration `yaml:"top_window"`     // top-URL/IP aggregation window
	TopKeep       int      `yaml:"top_keep"`       // rows kept per window
}

type RetentionConfig struct {
	MinuteDays int `yaml:"minute_days"` // per-minute samples
	DiskDays   int `yaml:"disk_days"`   // per-hour disk usage
	TopDays    int `yaml:"top_days"`    // top URL/IP windows
	EventDays  int `yaml:"event_days"`  // alert log
}

type AlertConfig struct {
	Rules   []AlertRule   `yaml:"rules"`
	Email   EmailConfig   `yaml:"email"`
	Webhook WebhookConfig `yaml:"webhook"`
}

type AlertRule struct {
	Name     string   `yaml:"name"`
	Metric   string   `yaml:"metric"` // site.cpu, site.mem, site.req, site.err5, system.cpu
	Site     string   `yaml:"site"`   // site user, "*" for all
	Op       string   `yaml:"op"`     // >, <, >=, <=
	Value    float64  `yaml:"value"`
	For      Duration `yaml:"for"`
	Cooldown Duration `yaml:"cooldown"`
	Severity string   `yaml:"severity"` // warning | critical
}

type EmailConfig struct {
	Enabled  bool     `yaml:"enabled"`
	Host     string   `yaml:"smtp_host"` // host:port
	Username string   `yaml:"username"`
	Password string   `yaml:"password"`
	From     string   `yaml:"from"`
	To       []string `yaml:"to"`
}

type WebhookConfig struct {
	Enabled bool   `yaml:"enabled"`
	URL     string `yaml:"url"`
}

// Save writes the config back to path as YAML (0600). Note: YAML comments
// in the file are lost.
func Save(path string, c *Config) error {
	out, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

// Load reads the YAML config at path and applies defaults for empty fields.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.applyDefaults()
	return c, nil
}

// ApplyDefaults fills empty fields (used when creating a fresh config).
func (c *Config) ApplyDefaults() { c.applyDefaults() }

func (c *Config) applyDefaults() {
	setStr := func(s *string, v string) {
		if *s == "" {
			*s = v
		}
	}
	setStr(&c.Listen, "127.0.0.1:8811")
	setStr(&c.DataDir, "/var/lib/website-stats")
	if c.SessionTTL.Duration == 0 {
		c.SessionTTL.Duration = 30 * 24 * time.Hour
	}
	setStr(&c.Collect.NginxSitesDir, "/etc/nginx/sites-enabled")
	setStr(&c.Collect.FpmPoolGlob, "/etc/php/*/fpm/pool.d/*.conf")
	setStr(&c.Collect.FpmStatusPath, "/wstats-fpm-status")
	if c.Collect.Interval.Duration == 0 {
		c.Collect.Interval.Duration = 10 * time.Second
	}
	if c.Collect.FpmInterval.Duration == 0 {
		c.Collect.FpmInterval.Duration = 30 * time.Second
	}
	if c.Collect.DiskInterval.Duration == 0 {
		c.Collect.DiskInterval.Duration = time.Hour
	}
	if c.Collect.DiscoverEvery.Duration == 0 {
		c.Collect.DiscoverEvery.Duration = 5 * time.Minute
	}
	if c.Collect.TopWindow.Duration == 0 {
		c.Collect.TopWindow.Duration = 10 * time.Minute
	}
	if c.Collect.TopKeep == 0 {
		c.Collect.TopKeep = 20
	}
	if c.Retention.MinuteDays == 0 {
		c.Retention.MinuteDays = 30
	}
	if c.Retention.DiskDays == 0 {
		c.Retention.DiskDays = 90
	}
	if c.Retention.TopDays == 0 {
		c.Retention.TopDays = 14
	}
	if c.Retention.EventDays == 0 {
		c.Retention.EventDays = 90
	}
	if len(c.Alerts.Rules) == 0 {
		c.Alerts.Rules = DefaultAlertRules()
	}
}

func DefaultAlertRules() []AlertRule {
	return []AlertRule{
		{Name: "site-high-cpu", Metric: "site.cpu", Site: "*", Op: ">", Value: 40, For: Duration{5 * time.Minute}, Cooldown: Duration{time.Hour}, Severity: "warning"},
		{Name: "site-5xx-spike", Metric: "site.err5", Site: "*", Op: ">", Value: 20, For: Duration{5 * time.Minute}, Cooldown: Duration{time.Hour}, Severity: "warning"},
		{Name: "system-high-cpu", Metric: "system.cpu", Site: "*", Op: ">", Value: 85, For: Duration{5 * time.Minute}, Cooldown: Duration{time.Hour}, Severity: "critical"},
		{Name: "system-high-load5", Metric: "system.load5", Site: "*", Op: ">", Value: 4, For: Duration{5 * time.Minute}, Cooldown: Duration{time.Hour}, Severity: "warning"},
	}
}
