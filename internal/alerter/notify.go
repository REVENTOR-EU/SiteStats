package alerter

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"time"

	"github.com/REVENTOR-EU/SiteStats/internal/config"
)

// sendMail delivers msg to all recipients. Ports 465 use implicit TLS
// (the server expects the TLS handshake before the SMTP greeting); other
// ports use STARTTLS when offered. Every connection gets a deadline so a
// silent server can't hang an alert dispatch.
func sendMail(cfg config.EmailConfig, from string, to []string, msg []byte) error {
	host, port, _ := strings.Cut(cfg.Host, ":")
	if port == "" {
		port = "587"
	}
	addr := net.JoinHostPort(host, port)

	deadline := time.Now().Add(30 * time.Second)
	var conn net.Conn
	var err error
	if port == "465" {
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: 15 * time.Second}, "tcp", addr,
			&tls.Config{ServerName: host})
	} else {
		conn, err = net.DialTimeout("tcp", addr, 15*time.Second)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(deadline)

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer c.Close()

	if err = c.Hello("localhost"); err != nil {
		return err
	}
	if port != "465" {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err = c.StartTLS(&tls.Config{ServerName: host}); err != nil {
				return err
			}
		}
	}
	if cfg.Username != "" {
		auth := smtp.PlainAuth("", cfg.Username, cfg.Password, host)
		if ok, _ := c.Extension("AUTH"); ok {
			if err = c.Auth(auth); err != nil {
				return fmt.Errorf("auth failed: %w", err)
			}
		}
	}
	if err = c.Mail(from); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err = c.Rcpt(rcpt); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err = w.Write(msg); err != nil {
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// NotifyEmail sends the alert via SMTP.
func NotifyEmail(cfg config.EmailConfig, ev Event) error {
	if !cfg.Enabled || len(cfg.To) == 0 {
		return nil
	}
	host := cfg.Host
	if !strings.Contains(host, ":") {
		host = host + ":587"
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

	return sendMail(cfg, from, cfg.To, []byte(msg))
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
