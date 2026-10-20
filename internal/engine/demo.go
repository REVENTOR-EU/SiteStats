package engine

import (
	"context"
	"math"
	"time"

	"github.com/REVENTOR-EU/SiteStats/internal/collect"
	"github.com/REVENTOR-EU/SiteStats/internal/sites"
)

type demoSim struct {
	baseCPU float64
	phase   float64
	speed   float64
	mem     float64
}

// RunDemo drives the collector with synthetic data instead of real
// collectors: five fake sites with rolling CPU/memory curves, a periodic
// spike on "shop", fabricated FPM pool stats and traffic counters.
// Useful to try the UI without attaching it to a server.
func (c *Collector) RunDemo(ctx context.Context) {
	list := []*sites.Site{
		{User: "shop", Domain: "shop-example.com", UID: 1, Home: "/home/shop"},
		{User: "blog", Domain: "blog-example.at", UID: 2, Home: "/home/blog"},
		{User: "forum", Domain: "forum-example.org", UID: 3, Home: "/home/forum"},
		{User: "crm", Domain: "crm-example.com", UID: 4, Home: "/home/crm"},
		{User: "docs", Domain: "docs-example.dev", UID: 5, Home: "/home/docs"},
	}
	reg := sites.NewEmpty()
	reg.SetSites(list)
	c.reg.Store(reg)

	sims := map[string]demoSim{
		"shop":  {baseCPU: 1.2, phase: 0.0, speed: 0.9, mem: 180},
		"blog":  {baseCPU: 0.6, phase: 1.1, speed: 0.5, mem: 240},
		"forum": {baseCPU: 0.4, phase: 2.4, speed: 1.7, mem: 310},
		"crm":   {baseCPU: 2.1, phase: 3.2, speed: 0.7, mem: 150},
		"docs":  {baseCPU: 0.2, phase: 4.0, speed: 1.2, mem: 60},
	}

	t := time.NewTicker(c.cfg.Collect.Interval.Duration)
	defer t.Stop()
	start := time.Now()
	lastMinute := int64(0)

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			elapsed := time.Since(start).Seconds()
			now := time.Now()
			snap := c.demoSnapshot(elapsed, sims, now)
			c.attachFpm(&snap)
			c.latest.Store(&snap)
			c.pushRing(snap)

			c.mu.Lock()
			for user := range snap.Sites {
				st := *snap.Sites[user].Fpm
				c.fpmNow[user] = &st
				c.fpmAccepted[user] = int64(elapsed * 10)
				if _, ok := c.fpmFlushed[user]; !ok {
					c.fpmFlushed[user] = c.fpmAccepted[user]
				}
			}
			c.mu.Unlock()

			minute := now.UTC().Truncate(time.Minute).Unix()
			if minute != lastMinute && lastMinute != 0 {
				c.demoWriteMinute(minute, snap)
			}
			lastMinute = minute
		}
	}
}

func (c *Collector) demoSnapshot(elapsed float64, sims map[string]demoSim, now time.Time) collect.Snapshot {
	list := c.reg.Load().Sites()
	snap := collect.Snapshot{Ts: now.Unix(), Sites: map[string]*collect.SiteLive{}}
	sysCPU := 0.0
	for _, s := range list {
		sm := sims[s.User]
		wave := math.Max(0, math.Sin(elapsed/sm.speed+sm.phase))
		spike := 0.0
		if math.Mod(elapsed/45, 4) < 0.4 { // periodic short spike on "shop"
			spike = 16
		}
		cpu := demoClamp(sm.baseCPU + wave*(4+float64(len(s.User))) + spike*demoBoolToFloat(s.User == "shop"))
		mem := sm.mem + wave*40
		snap.Sites[s.User] = &collect.SiteLive{
			CPU:   cpu,
			MemMB: mem,
			Procs: 2 + int(wave*3),
			Top: []collect.ProcInfo{
				{PID: 1000 + int(elapsed), Name: "php-fpm: pool " + s.Domain, CPU: cpu * 0.8, MemMB: mem * 0.7},
			},
			Fpm: &collect.FpmStatus{
				Active:       int(wave*3) + 1,
				Idle:         2,
				TotalProcs:   4,
				ListenQueue:  int(wave * 1.5),
				AcceptedConn: int64(elapsed * 10),
			},
		}
		sysCPU += cpu
	}
	snap.System = collect.SystemLive{
		CPU:         demoClamp(sysCPU * 0.85),
		MemUsedMB:   3400 + math.Sin(elapsed/30)*120,
		MemTotalMB:  7929,
		Load1:       0.4 + sysCPU/12,
		Load5:       0.4 + sysCPU/14,
		Load15:      0.4 + sysCPU/16,
		NetRx:       60000 + sysCPU*3000,
		NetTx:       90000 + sysCPU*5000,
		DiskUsedGB:  87,
		DiskTotalGB: 503,
	}
	return snap
}

// demoWriteMinute assembles and stores the per-minute rollup for the
// synthetic sites and feeds the alerter.
func (c *Collector) demoWriteMinute(minuteTs int64, snap collect.Snapshot) {
	rows := make([]collect.MinuteRow, 0, len(snap.Sites)+1)
	for user, sl := range snap.Sites {
		row := collect.MinuteRow{
			Ts: minuteTs, Site: user,
			CPU: sl.CPU, MemMB: sl.MemMB, Procs: sl.Procs,
			Req: int64(20 + sl.CPU*30),
		}
		if sl.Fpm != nil {
			row.FpmActive = sl.Fpm.Active
			row.FpmIdle = sl.Fpm.Idle
			row.FpmQueue = sl.Fpm.ListenQueue
			row.FpmReq = int64(sl.Fpm.Active * 12)
		}
		rows = append(rows, row)
	}
	sys := collect.MinuteRow{Ts: minuteTs, Site: "_system", CPU: snap.System.CPU, MemMB: snap.System.MemUsedMB}
	rows = append(rows, sys)

	var memTotalMB float64
	if snap.System.MemTotalMB > 0 {
		memTotalMB = snap.System.MemTotalMB
	}
	for _, ev := range c.aler.Evaluate(rows, memTotalMB) {
		c.store.InsertEvent(ev.Ts, "alert", ev.Site, ev.Rule, ev.Message)
	}
	_ = c.store.InsertMinute(rows)
}

func demoClamp(v float64) float64 {
	if v > 100 {
		return 100
	}
	if v < 0 {
		return 0
	}
	return v
}

func demoBoolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
