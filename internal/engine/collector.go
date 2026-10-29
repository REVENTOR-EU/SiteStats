// Package engine orchestrates all collectors: it runs them on tickers,
// assembles per-minute rollups into the store and feeds the alerter.
package engine

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/REVENTOR-EU/SiteStats/internal/alerter"
	"github.com/REVENTOR-EU/SiteStats/internal/collect"
	"github.com/REVENTOR-EU/SiteStats/internal/config"
	"github.com/REVENTOR-EU/SiteStats/internal/sites"
	"github.com/REVENTOR-EU/SiteStats/internal/store"
)

// Collector runs all collectors on their tickers and assembles per-minute
// rollups into the store.
type Collector struct {
	cfg   *config.Config
	store *store.Store
	aler  *alerter.Alerter

	reg    atomic.Pointer[sites.Registry]
	latest atomic.Pointer[collect.Snapshot]
	tailer *collect.LogTailer
	ps     *collect.ProcState

	mu          sync.Mutex
	fpmNow      map[string]*collect.FpmStatus // user -> summed pool status
	fpmAccepted map[string]int64              // user -> cumulative accepted conns
	fpmFlushed  map[string]int64              // user -> accepted conns at last flush
	ring        []collect.Snapshot
	diskUsedGB  float64
	diskTotalGB float64
}

func NewCollector(cfg *config.Config, st *store.Store, al *alerter.Alerter) *Collector {
	c := &Collector{
		cfg:         cfg,
		store:       st,
		aler:        al,
		ps:          collect.NewProcState(),
		fpmNow:      map[string]*collect.FpmStatus{},
		fpmAccepted: map[string]int64{},
		fpmFlushed:  map[string]int64{},
	}
	// alert messages should name the actual website, not the linux user
	al.SetNameFn(func(user string) string {
		if user == "_sys" {
			return "system services (non-site processes, e.g. mysql)"
		}
		if user == "_system" {
			return "server"
		}
		if reg := c.reg.Load(); reg != nil {
			if s := reg.Get(user); s != nil && s.Domain != "" {
				return fmt.Sprintf("%s (%s)", user, s.Domain)
			}
		}
		return user
	})
	return c
}

func (c *Collector) Registry() *sites.Registry { return c.reg.Load() }
func (c *Collector) Latest() *collect.Snapshot { return c.latest.Load() }

// LiveHits returns the site's recent individual requests from the log tailer.
func (c *Collector) LiveHits(user string) []collect.LiveHit {
	if c.tailer == nil {
		return nil
	}
	return c.tailer.LiveHits(user)
}

// FpmFor returns a copy of the site's last known pooled FPM status.
func (c *Collector) FpmFor(user string) *collect.FpmStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f, ok := c.fpmNow[user]; ok && f != nil {
		fc := *f
		return &fc
	}
	return nil
}

// Run blocks until ctx is cancelled, running all collection loops.
func (c *Collector) Run(ctx context.Context) {
	reg, err := sites.Discover(c.cfg.Collect.NginxSitesDir, c.cfg.Collect.FpmPoolGlob)
	if err != nil {
		log.Printf("site discovery failed: %v", err)
		reg = sites.NewEmpty()
	}
	c.reg.Store(reg)
	c.tailer = collect.NewLogTailer(reg, c.cfg.Collect.TopWindow.Duration, c.cfg.Collect.TopKeep)
	go c.tailer.Run(ctx)
	go c.collectDisk(ctx) // first reading without waiting an hour

	discover := time.NewTicker(c.cfg.Collect.DiscoverEvery.Duration)
	defer discover.Stop()
	go func() {
		for range discover.C {
			if ctx.Err() != nil {
				return
			}
			nreg, err := sites.Discover(c.cfg.Collect.NginxSitesDir, c.cfg.Collect.FpmPoolGlob)
			if err != nil {
				continue
			}
			c.reg.Store(nreg)
			c.tailer.SetRegistry(nreg)
		}
	}()

	procDone := make(chan struct{})
	go func() {
		defer close(procDone)
		t := time.NewTicker(c.cfg.Collect.Interval.Duration)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				snap := collect.ScanProc(c.ps, c.reg.Load())
				c.attachFpm(&snap)
				c.latest.Store(&snap)
				c.pushRing(snap)
			}
		}
	}()

	fpmDone := make(chan struct{})
	go func() {
		defer close(fpmDone)
		t := time.NewTicker(c.cfg.Collect.FpmInterval.Duration)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.pollFpm()
			}
		}
	}()

	diskDone := make(chan struct{})
	go func() {
		defer close(diskDone)
		t := time.NewTicker(c.cfg.Collect.DiskInterval.Duration)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.collectDisk(ctx)
			}
		}
	}()

	pruneDone := make(chan struct{})
	go func() {
		defer close(pruneDone)
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				r := c.cfg.Retention
				if err := c.store.Prune(r.MinuteDays, r.DiskDays, r.TopDays, r.EventDays); err != nil {
					log.Printf("prune: %v", err)
				}
			}
		}
	}()

	minute := time.NewTicker(time.Second)
	defer minute.Stop()
	lastMinute := int64(0)
	for {
		select {
		case <-ctx.Done():
			<-procDone
			<-fpmDone
			<-diskDone
			<-pruneDone
			return
		case <-minute.C:
			m := time.Now().UTC().Truncate(time.Minute).Unix()
			if m != lastMinute {
				lastMinute = m
				c.flushMinute(m)
			}
		}
	}
}

func (c *Collector) pushRing(snap collect.Snapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ring = append(c.ring, snap)
	max := int(time.Hour / c.cfg.Collect.Interval.Duration)
	if max < 6 {
		max = 6
	}
	if len(c.ring) > max {
		c.ring = c.ring[len(c.ring)-max:]
	}
}

// flushMinute assembles per-site minute rows from the log tailer, the proc
// ring and the FPM cache, then persists them and feeds the alerter.
func (c *Collector) flushMinute(minuteTs int64) {
	traffic := c.tailer.DrainMinute(minuteTs)

	c.mu.Lock()
	ring := c.ring
	fpmNow := c.fpmNow
	fpmAccepted := c.fpmAccepted
	fpmFlushed := c.fpmFlushed
	c.mu.Unlock()

	// average CPU over the minute from ring snapshots within the minute window
	from := minuteTs - 60
	type acc struct {
		cpuSum float64
		n      int
		mem    float64
		procs  int
	}
	perSite := map[string]*acc{}
	sysCPU := acc{}
	for _, s := range ring {
		if s.Ts <= from || s.Ts > minuteTs {
			continue
		}
		sysCPU.cpuSum += s.System.CPU
		sysCPU.n++
		for user, sl := range s.Sites {
			a := perSite[user]
			if a == nil {
				a = &acc{}
				perSite[user] = a
			}
			a.cpuSum += sl.CPU
			a.n++
			a.mem = sl.MemMB
			a.procs = sl.Procs
		}
	}

	// fold all "_sys:*" pseudo-sites into one "System services" row
	sysAgg := acc{}
	for user, a := range perSite {
		if len(user) > 5 && user[:5] == "_sys:" {
			sysAgg.cpuSum += a.cpuSum
			sysAgg.n += a.n
			sysAgg.mem += a.mem
			sysAgg.procs += a.procs
			delete(perSite, user)
		}
	}
	if sysAgg.n > 0 {
		perSite["_sys"] = &sysAgg
	}

	keys := map[string]bool{}
	for k := range perSite {
		keys[k] = true
	}
	for k := range traffic {
		keys[k] = true
	}
	for k := range fpmNow {
		keys[k] = true
	}
	delete(keys, "_system")

	rows := make([]collect.MinuteRow, 0, len(keys)+1)
	for user := range keys {
		row := collect.MinuteRow{Ts: minuteTs, Site: user}
		if a := perSite[user]; a != nil && a.n > 0 {
			row.CPU = a.cpuSum / float64(a.n)
			row.MemMB = a.mem
			row.Procs = a.procs
		}
		if t := traffic[user]; t != nil {
			row.Req, row.Bytes, row.R4xx, row.R5xx = t.Req, t.Bytes, t.R4xx, t.R5xx
		}
		if f := fpmNow[user]; f != nil {
			row.FpmActive = f.Active
			row.FpmIdle = f.Idle
			row.FpmQueue = f.ListenQueue
			row.FpmReq = fpmAccepted[user] - fpmFlushed[user]
			if row.FpmReq < 0 {
				row.FpmReq = 0
			}
		}
		rows = append(rows, row)
	}
	// system pseudo-row (whole host)
	sysRow := collect.MinuteRow{Ts: minuteTs, Site: "_system"}
	if sysCPU.n > 0 {
		sysRow.CPU = sysCPU.cpuSum / float64(sysCPU.n)
	}
	if snap := c.latest.Load(); snap != nil {
		sysRow.MemMB = snap.System.MemUsedMB
		sysRow.Load1 = snap.System.Load1
		sysRow.Load5, sysRow.Load15 = snap.System.Load5, snap.System.Load15
	}
	rows = append(rows, sysRow)

	for k := range fpmFlushed {
		fpmFlushed[k] = fpmAccepted[k]
	}

	if err := c.store.InsertMinute(rows); err != nil {
		log.Printf("insert minute: %v", err)
	}
	var memTotalMB float64
	if snap := c.latest.Load(); snap != nil {
		memTotalMB = snap.System.MemTotalMB
	}
	for _, ev := range c.aler.Evaluate(rows, memTotalMB) {
		c.store.InsertEvent(ev.Ts, "alert", ev.Site, ev.Rule, ev.Message)
		go notifyAll(c.cfg.Alerts, ev)
	}
	if win, ok := c.tailer.DrainTop(); ok {
		if err := c.store.InsertTopWindow(win); err != nil {
			log.Printf("insert top window: %v", err)
		}
	}
}

// notifyAll dispatches an alert to every enabled channel.
func notifyAll(cfg config.AlertConfig, ev alerter.Event) {
	if err := alerter.NotifyEmail(cfg.Email, ev); err != nil {
		log.Printf("alert email: %v", err)
	}
	if err := alerter.NotifyWebhook(cfg.Webhook, ev); err != nil {
		log.Printf("alert webhook: %v", err)
	}
}

// attachFpm copies the cached FPM status into the live snapshot.
func (c *Collector) attachFpm(snap *collect.Snapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for user, f := range c.fpmNow {
		if sl, ok := snap.Sites[user]; ok {
			fc := *f
			sl.Fpm = &fc
		}
	}
	snap.System.DiskUsedGB = c.diskUsedGB
	snap.System.DiskTotalGB = c.diskTotalGB
}

// pollFpm queries every site's pool status endpoints concurrently. Pool
// accepted-conn counters are cumulative, so the per-user sum replaces the
// previous value; deltas are taken at minute flushes.
func (c *Collector) pollFpm() {
	reg := c.reg.Load()
	type res struct {
		user string
		st   *collect.FpmStatus
	}
	var wg sync.WaitGroup
	out := make(chan res, 64)
	for _, site := range reg.Sites() {
		if len(site.Pools) == 0 {
			continue
		}
		for _, pool := range site.Pools {
			wg.Add(1)
			go func(user string, p sites.PoolAddr) {
				defer wg.Done()
				st, err := collect.QueryFpmStatus(p.Addr, p.StatusPath, 5*time.Second)
				if err != nil {
					return // pool down or status page missing: silently skip
				}
				out <- res{user, st}
			}(site.User, pool)
		}
	}
	go func() {
		wg.Wait()
		close(out)
	}()

	sums := map[string]*collect.FpmStatus{}
	accepted := map[string]int64{}
	for r := range out {
		cur := sums[r.user]
		if cur == nil {
			cur = &collect.FpmStatus{}
			sums[r.user] = cur
		}
		cur.Active += r.st.Active
		cur.Idle += r.st.Idle
		cur.TotalProcs += r.st.TotalProcs
		cur.ListenQueue += r.st.ListenQueue
		if r.st.MaxListenQueue > cur.MaxListenQueue {
			cur.MaxListenQueue = r.st.MaxListenQueue
		}
		if r.st.MaxChildren > cur.MaxChildren {
			cur.MaxChildren = r.st.MaxChildren
		}
		cur.SlowRequests += r.st.SlowRequests
		accepted[r.user] += r.st.AcceptedConn
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for user, st := range sums {
		c.fpmNow[user] = st
		if prev, ok := c.fpmAccepted[user]; !ok || accepted[user] < prev {
			// first sight or FPM restart (counter reset): resync baseline
			c.fpmFlushed[user] = accepted[user]
		}
		c.fpmAccepted[user] = accepted[user]
	}
	for user := range c.fpmNow {
		if _, ok := sums[user]; !ok {
			delete(c.fpmNow, user)
		}
	}
}

// collectDisk stores hourly du for every site home and the filesystem total.
func (c *Collector) collectDisk(ctx context.Context) {
	reg := c.reg.Load()
	now := time.Now().UTC().Truncate(time.Hour).Unix()
	var wg sync.WaitGroup
	for _, site := range reg.Sites() {
		if site.Home == "" {
			continue
		}
		wg.Add(1)
		go func(user, home string) {
			defer wg.Done()
			if bytes, err := collect.DiskUsage(ctx, home, 10*time.Minute); err == nil {
				c.store.InsertDisk(now, user, bytes)
			}
		}(site.User, site.Home)
	}
	wg.Wait()
	if used, total, err := collect.FilesystemUsage("/"); err == nil && total > 0 {
		c.store.InsertDisk(now, "_system", int64(total))
		c.store.InsertDisk(now, "_system_used", int64(used))
		c.mu.Lock()
		c.diskTotalGB = float64(total) / (1 << 30)
		c.diskUsedGB = float64(used) / (1 << 30)
		c.mu.Unlock()
	}
}
