package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/REVENTOR-EU/SiteStats/internal/alerter"
	"github.com/REVENTOR-EU/SiteStats/internal/config"
	"github.com/REVENTOR-EU/SiteStats/internal/engine"
	"github.com/REVENTOR-EU/SiteStats/internal/store"
)

func testServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "wstats-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfgPath := dir + "/config.yml"
	cfg := &config.Config{DataDir: dir}
	cfg.ApplyDefaults()
	cfg.PasswordHash = "x"
	cfg.Alerts.Email = config.EmailConfig{
		Enabled: true, Host: "smtp.example.com:587", Username: "u",
		Password: "saved-secret", From: "from@example.com", To: []string{"to@example.com"},
	}
	al := alerter.New(cfg.Alerts)
	return New(cfg, st, engine.NewCollector(cfg, st, al), al, cfgPath), cfgPath
}

func TestSettingsGetRedactsPassword(t *testing.T) {
	s, _ := testServer(t)
	w := httptest.NewRecorder()
	s.handleSettingsGet(w, httptest.NewRequest("GET", "/api/settings", nil))
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	var body map[string]any
	json.NewDecoder(w.Body).Decode(&body)
	out, _ := json.Marshal(body)
	if strings.Contains(string(out), "saved-secret") {
		t.Fatalf("response leaks SMTP password: %s", out)
	}
	email := body["email"].(map[string]any)
	if email["hasPassword"] != true {
		t.Fatalf("expected hasPassword=true, got %v", email)
	}
}

func TestSettingsSavePersistsAndApplies(t *testing.T) {
	s, cfgPath := testServer(t)
	payload := settingsJSON{}
	payload.Email.Enabled = true
	payload.Email.SmtpHost = "mail2.example.com"
	payload.Email.From = "a@b.c"
	payload.Email.To = []string{"x@y.z"}
	payload.Email.Password = "new-secret"
	payload.Email.To = []string{"x@y.z"}
	payload.Rules = []ruleJSON{
		{Name: "cpu-rule", Metric: "site.cpu", Site: "*", Op: ">", Value: 50, For: "2m", Cooldown: "30m", Severity: "critical"},
		{Name: "bad", Metric: "nope", Op: ">", Value: 1},
	}
	b, _ := json.Marshal(payload)
	w := httptest.NewRecorder()
	s.handleSettingsSave(w, httptest.NewRequest("POST", "/api/settings", bytes.NewReader(b)))
	if w.Code != 400 {
		t.Fatalf("invalid metric should be rejected, got status %d", w.Code)
	}

	payload.Rules = payload.Rules[:1]
	b, _ = json.Marshal(payload)
	w = httptest.NewRecorder()
	s.handleSettingsSave(w, httptest.NewRequest("POST", "/api/settings", bytes.NewReader(b)))
	if w.Code != 200 {
		t.Fatalf("save failed: %d %s", w.Code, w.Body.String())
	}

	// applied live
	if s.cfg.Alerts.Email.Host != "mail2.example.com:587" {
		t.Fatalf("live config not updated: %q", s.cfg.Alerts.Email.Host)
	}
	// submitted password replaced the stored one
	if s.cfg.Alerts.Email.Password != "new-secret" {
		t.Fatalf("submitted password not saved: %q", s.cfg.Alerts.Email.Password)
	}
	// alerter rules swapped
	if len(s.al.Evaluate(nil, 1000)) != 0 {
		t.Fatal("Evaluate must still work after config swap")
	}
	// persisted to disk
	disk, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(disk.Alerts.Rules) != 1 || disk.Alerts.Rules[0].Name != "cpu-rule" {
		t.Fatalf("rules not persisted: %+v", disk.Alerts.Rules)
	}
	if disk.Alerts.Rules[0].For.Duration != 2*time.Minute {
		t.Fatalf("for duration not persisted: %+v", disk.Alerts.Rules[0].For)
	}
}

func TestTestEmailRequiresHostAndRecipient(t *testing.T) {
	s, _ := testServer(t)
	payload := settingsJSON{}
	payload.Email.Enabled = true
	b, _ := json.Marshal(payload)
	w := httptest.NewRecorder()
	s.handleTestEmail(w, httptest.NewRequest("POST", "/api/settings/test-email", bytes.NewReader(b)))
	if w.Code != 400 {
		t.Fatalf("missing host/recipient should 400, got %d", w.Code)
	}
}
