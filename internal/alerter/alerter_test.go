package alerter

import (
	"testing"
	"time"

	"github.com/REVENTOR-EU/SiteStats/internal/collect"
	"github.com/REVENTOR-EU/SiteStats/internal/config"
)

func rule(name, metric string, value float64) config.AlertRule {
	return config.AlertRule{
		Name: name, Metric: metric, Site: "*", Op: ">", Value: value,
		For:      config.Duration{Duration: time.Minute},
		Cooldown: config.Duration{Duration: time.Hour},
		Severity: "warning",
	}
}

func row(site string, cpu float64) collect.MinuteRow {
	return collect.MinuteRow{Site: site, CPU: cpu}
}

func TestEvaluateFiresAfterFor(t *testing.T) {
	now := int64(1000)
	a := New(config.AlertConfig{Rules: []config.AlertRule{rule("r", "site.cpu", 40)}})
	a.nowFn = func() int64 { return now }

	if ev := a.Evaluate([]collect.MinuteRow{row("site1", 90)}, 1000); len(ev) != 0 {
		t.Fatalf("first breach must not fire, got %d events", len(ev))
	}
	now += 61
	ev := a.Evaluate([]collect.MinuteRow{row("site1", 90)}, 1000)
	if len(ev) != 1 || ev[0].Site != "site1" || ev[0].Rule != "r" {
		t.Fatalf("expected one event for site1, got %+v", ev)
	}
	// cooldown blocks a re-fire while still breaching
	now += 61
	if ev := a.Evaluate([]collect.MinuteRow{row("site1", 90)}, 1000); len(ev) != 0 {
		t.Fatalf("cooldown must suppress re-fire, got %d events", len(ev))
	}
	// recovery clears breach state
	now += 3600
	a.Evaluate([]collect.MinuteRow{row("site1", 10)}, 1000)
	now += 61
	if ev := a.Evaluate([]collect.MinuteRow{row("site1", 90)}, 1000); len(ev) != 0 {
		t.Fatalf("breach after recovery starts fresh, got %d events", len(ev))
	}
	now += 61
	if ev := a.Evaluate([]collect.MinuteRow{row("site1", 90)}, 1000); len(ev) != 1 {
		t.Fatalf("expected re-fire after recovery, got %d events", len(ev))
	}
}

func TestLoadMetricFiresOnSystemRow(t *testing.T) {
	now := int64(1000)
	a := New(config.AlertConfig{Rules: []config.AlertRule{rule("load", "system.load5", 4)}})
	a.nowFn = func() int64 { return now }
	sys := collect.MinuteRow{Site: "_system", Load5: 6.2}
	site := collect.MinuteRow{Site: "site1", CPU: 90}

	a.Evaluate([]collect.MinuteRow{sys, site}, 1000)
	now += 61
	ev := a.Evaluate([]collect.MinuteRow{sys, site}, 1000)
	if len(ev) != 1 || ev[0].Site != "_system" {
		t.Fatalf("expected load alert on _system, got %+v", ev)
	}
}

// Regression: New() once froze its clock via a time.Now().Unix method value,
// so breach timers never elapsed and no alert could ever fire.
func TestDefaultClockAdvances(t *testing.T) {
	a := New(config.AlertConfig{})
	t1 := a.nowFn()
	time.Sleep(1100 * time.Millisecond)
	if a.nowFn() <= t1 {
		t.Fatalf("default nowFn must return a moving clock")
	}
}

// "*" rules cover real websites only; _sys needs an explicit rule
func TestWildcardSkipsSystemServices(t *testing.T) {
	now := int64(1000)
	a := New(config.AlertConfig{Rules: []config.AlertRule{rule("mem", "site.mem", 1000)}})
	a.nowFn = func() int64 { return now }
	a.Evaluate([]collect.MinuteRow{{Site: "_sys", MemMB: 3000}}, 8000)
	now += 61
	if ev := a.Evaluate([]collect.MinuteRow{{Site: "_sys", MemMB: 3000}}, 8000); len(ev) != 0 {
		t.Fatalf("wildcard rule must not fire for _sys, got %+v", ev)
	}
	a.UpdateConfig(config.AlertConfig{Rules: []config.AlertRule{rule("mem", "site.mem", 1000),
		{Name: "sys-mem", Metric: "site.mem", Site: "_sys", Op: ">", Value: 1000,
			For: config.Duration{Duration: time.Minute}, Cooldown: config.Duration{Duration: time.Hour}, Severity: "critical"}}})
	a.Evaluate([]collect.MinuteRow{{Site: "_sys", MemMB: 3000}}, 8000)
	now += 61
	if ev := a.Evaluate([]collect.MinuteRow{{Site: "_sys", MemMB: 3000}}, 8000); len(ev) != 1 {
		t.Fatalf("explicit _sys rule should fire, got %+v", ev)
	}
}

func TestUpdateConfigSwapsRulesAndClearsBreach(t *testing.T) {
	now := int64(1000)
	a := New(config.AlertConfig{Rules: []config.AlertRule{rule("r", "site.cpu", 40)}})
	a.nowFn = func() int64 { return now }

	a.Evaluate([]collect.MinuteRow{row("site1", 90)}, 1000)
	a.UpdateConfig(config.AlertConfig{Rules: []config.AlertRule{rule("other", "site.err5", 5)}})
	a.mu.Lock()
	n := len(a.breach)
	a.mu.Unlock()
	if n != 0 {
		t.Fatalf("UpdateConfig must wipe breach state, %d keys left", n)
	}
	// the old rule no longer fires, the new one tracks its own breach
	now += 61
	if ev := a.Evaluate([]collect.MinuteRow{row("site1", 90)}, 1000); len(ev) != 0 {
		t.Fatalf("removed rule must not fire after UpdateConfig")
	}
	if ev := a.Evaluate([]collect.MinuteRow{{Site: "site1", R5xx: 50}}, 1000); len(ev) != 0 {
		t.Fatalf("first breach of the new rule must not fire yet, got %+v", ev)
	}
	now += 61
	if ev := a.Evaluate([]collect.MinuteRow{{Site: "site1", R5xx: 50}}, 1000); len(ev) != 1 {
		t.Fatalf("new rule should fire, got %+v", ev)
	}
}

func TestUpdateConfigAppliesDefaults(t *testing.T) {
	a := New(config.AlertConfig{})
	a.UpdateConfig(config.AlertConfig{})
	a.mu.Lock()
	n := len(a.cfg.Rules)
	a.mu.Unlock()
	if n != len(config.DefaultAlertRules()) {
		t.Fatalf("empty rule set must fall back to defaults, got %d", n)
	}
}
