package alerter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/smtp"
	"strings"
	"time"

	"website-stats/internal/config"
)

// NotifyEmail sends the alert via SMTP (STARTTLS when offered).
func NotifyEmail(cfg config.EmailConfig, ev Event) error {
	if !cfg.Enabled || len(cfg.To) == 0 {
		return nil
	}
	host := cfg.Host
	addr := host
	if !strings.Contains(host, ":") {
		addr = host + ":587"
	}
	subject := fmt.Sprintf("[website-stats][%s] %s on %s", ev.Severity, ev.Rule, ev.Site)
	body := fmt.Sprintf("Time: %s\nRule: %s\nSite: %s\n\n%s\n",
		time.Unix(ev.Ts, 0).Format(time.RFC1123Z), ev.Rule, ev.Site, ev.Message)

	from := cfg.From
	if from == "" {
		from = cfg.Username
	}
	msg := strings.Join([]string{
		"From: " + from,
		"To: " + strings.Join(cfg.To, ", "),
		"Subject: " + subject,
		"Content-Type: text/plain; charset=utf-8",
		"",
		body,
	}, "\r\n")

	var auth smtp.Auth
	if cfg.Username != "" {
		hostOnly, _, _ := strings.Cut(host, ":")
		auth = smtp.PlainAuth("", cfg.Username, cfg.Password, hostOnly)
	}
	return smtp.SendMail(addr, auth, from, cfg.To, []byte(msg))
}

// NotifyWebhook posts the alert as JSON to the configured URL.
func NotifyWebhook(cfg config.WebhookConfig, ev Event) error {
	if !cfg.Enabled || cfg.URL == "" {
		return nil
	}
	payload := map[string]any{
		"source":   "website-stats",
		"ts":       ev.Ts,
		"rule":     ev.Rule,
		"site":     ev.Site,
		"severity": ev.Severity,
		"message":  ev.Message,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(cfg.URL, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %s", resp.Status)
	}
	return nil
}
