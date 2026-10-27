// Package alerter evaluates per-site alert rules against minute rollups and
// dispatches notifications.
package alerter

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/REVENTOR-EU/SiteStats/internal/collect"
	"github.com/REVENTOR-EU/SiteStats/internal/config"
)

// Event is a fired alert.
type Event struct {
	Ts       int64
	Rule     string
	Site     string
	Severity string
	Message  string
}

type breachState struct {
	since    int64 // unix ts of first breach
	lastFire int64
}

// Alerter holds rule state (breach tracking) across evaluations.
type Alerter struct {
	mu     sync.Mutex
	cfg    config.AlertConfig
	breach map[string]*breachState // ruleName|site -> state
	nowFn  func() int64
}

func New(cfg config.AlertConfig) *Alerter {
	if len(cfg.Rules) == 0 {
		cfg.Rules = config.DefaultAlertRules()
	}
	// note: a method value like time.Now().Unix would freeze the clock at
	// construction time — the closure re-reads the clock on every call
	return &Alerter{cfg: cfg, breach: map[string]*breachState{}, nowFn: func() int64 { return time.Now().Unix() }}
}

// UpdateConfig swaps the rule set at runtime and wipes breach tracking so
// stale rule|site states don't survive a rule edit.
func (a *Alerter) UpdateConfig(cfg config.AlertConfig) {
	if len(cfg.Rules) == 0 {
		cfg.Rules = config.DefaultAlertRules()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg = cfg
	a.breach = map[string]*breachState{}
}

// metricValue extracts the rule metric from a minute row.
func metricValue(metric string, row *collect.MinuteRow, memTotalMB float64) (float64, bool) {
	switch metric {
	case "site.cpu":
		return row.CPU, true
	case "site.mem":
		return row.MemMB, true
	case "site.mem_pct":
		if memTotalMB <= 0 {
			return 0, false
		}
		return row.MemMB / memTotalMB * 100, true
	case "site.req":
		return float64(row.Req), true
	case "site.err5":
		return float64(row.R5xx), true
	case "system.cpu":
		return row.CPU, true
	case "system.load1":
		return row.Load1, true
	case "system.load5":
		return row.Load5, true
	case "system.load15":
		return row.Load15, true
	}
	return 0, false
}

func (a *Alerter) compare(op string, v, threshold float64) bool {
	switch op {
	case ">":
		return v > threshold
	case ">=":
		return v >= threshold
	case "<":
		return v < threshold
	case "<=":
		return v <= threshold
	}
	return false
}

// Evaluate checks every rule against the minute rollups. Rows carry site data
// plus the "_system" pseudo-row. Returns events to notify.
func (a *Alerter) Evaluate(rows []collect.MinuteRow, memTotalMB float64) []Event {
	now := a.nowFn()
	a.mu.Lock()
	defer a.mu.Unlock()
	var fired []Event
	seen := map[string]bool{}

	for _, row := range rows {
		for _, rule := range a.cfg.Rules {
			if rule.Site != "*" && rule.Site != row.Site {
				continue
			}
			isSystem := strings.HasPrefix(rule.Metric, "system.")
			if isSystem && row.Site != "_system" {
				continue
			}
			if !isSystem && row.Site == "_system" {
				continue
			}
			key := rule.Name + "|" + row.Site
			v, ok := metricValue(rule.Metric, &row, memTotalMB)
			if !ok {
				continue
			}
			seen[key] = true
			st := a.breach[key]
			if a.compare(rule.Op, v, rule.Value) {
				if st == nil {
					a.breach[key] = &breachState{since: now}
					continue
				}
				breachingFor := now - st.since
				if breachingFor >= int64(rule.For.Seconds()) &&
					(st.lastFire == 0 || now-st.lastFire >= int64(rule.Cooldown.Seconds())) {
					st.lastFire = now
					ev := Event{
						Ts:       now,
						Rule:     rule.Name,
						Site:     row.Site,
						Severity: rule.Severity,
						Message: fmt.Sprintf("%s: %s %.1f (threshold %s %.1f for %s)",
							rule.Name, displayName(row.Site, rule.Metric), v, rule.Op, rule.Value, rule.For),
					}
					fired = append(fired, ev)
				}
			} else {
				delete(a.breach, key)
			}
		}
	}
	// clear breaches for sites/rules that vanished from this evaluation
	for key := range a.breach {
		if !seen[key] {
			delete(a.breach, key)
		}
	}
	return fired
}

func displayName(site, metric string) string {
	if strings.HasPrefix(metric, "system.") {
		return "system " + strings.TrimPrefix(metric, "system.")
	}
	return fmt.Sprintf("site %s", site)
}
