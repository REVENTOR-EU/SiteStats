//go:build linux

package collect

import (
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/REVENTOR-EU/SiteStats/internal/sites"
)

const clkTck = 100 // kernel USER_HZ on all supported platforms

func runtimeNumCPU() int { return runtime.NumCPU() }

// ProcState is the exported handle for the scanner state (see scanProc).
type ProcState = procState

func NewProcState() *ProcState { return newProcState() }

// ScanProc performs one full /proc scan and computes per-user CPU/RAM.
func ScanProc(st *ProcState, reg *sites.Registry) Snapshot { return scanProc(st, reg) }

type procState struct {
	prevPid   map[int]uint64 // pid -> cumulative jiffies at last scan
	prevTotal uint64         // /proc/stat total jiffies
	prevIdle  uint64
	prevNetRx uint64
	prevNetTx uint64
	lastTs    time.Time
	procs     map[int]*pidInfo // pid -> cached uid/name for delta attribution
	ncores    int
	pageSize  int
	first     bool
}

type pidInfo struct {
	uid  int
	name string
	rss  uint64
}

func newProcState() *procState {
	return &procState{
		prevPid:  map[int]uint64{},
		procs:    map[int]*pidInfo{},
		ncores:   runtimeNumCPU(),
		pageSize: os.Getpagesize(),
		first:    true,
	}
}

// scanProc reads /proc and produces a Snapshot, computing per-process CPU as
// the delta of cumulative jiffies between scans.
func scanProc(st *procState, reg *sites.Registry) Snapshot {
	now := time.Now()
	snap := Snapshot{Ts: now.Unix(), Sites: map[string]*SiteLive{}}

	// System-wide CPU.
	if b, err := os.ReadFile("/proc/stat"); err == nil {
		total, idle, err := parseStatTotal(string(b))
		if err == nil && st.lastTs.After(time.Time{}) && total > st.prevTotal {
			busy := float64(total-st.prevTotal-(idle-st.prevIdle)) / float64(total-st.prevTotal)
			snap.System.CPU = clampPct(busy * 100)
		}
		st.prevTotal, st.prevIdle = total, idle
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		t, a := parseMeminfo(string(b))
		snap.System.MemTotalMB = float64(t) / 1024
		snap.System.MemUsedMB = float64(t-a) / 1024
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		snap.System.Load1 = parseLoadavg(string(b))
	}
	if b, err := os.ReadFile("/proc/net/dev"); err == nil {
		rx, tx := parseNetDev(string(b))
		if !st.first && st.lastTs.After(time.Time{}) {
			dt := now.Sub(st.lastTs).Seconds()
			if rx >= st.prevNetRx {
				snap.System.NetRx = float64(rx-st.prevNetRx) / dt
			}
			if tx >= st.prevNetTx {
				snap.System.NetTx = float64(tx-st.prevNetTx) / dt
			}
		}
		st.prevNetRx, st.prevNetTx = rx, tx
	}

	dt := now.Sub(st.lastTs).Seconds()
	first := st.first
	st.first = false
	st.lastTs = now

	perUid := map[int]*uidAgg{}
	seen := map[int]uint64{}

	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // process exited
		}
		comm, jiffies, rssPages, err := parsePidStat(string(b))
		if err != nil {
			continue
		}

		info, known := st.procs[pid]
		if !known {
			uid := -1
			name := comm
			if sb, err := os.ReadFile("/proc/" + e.Name() + "/status"); err == nil {
				uid, _ = parseUid(string(sb))
			}
			if cb, err := os.ReadFile("/proc/" + e.Name() + "/cmdline"); err == nil {
				if n := parseCmdline(string(cb)); n != "" {
					name = n
				}
			}
			if name == "" {
				name = strconv.Itoa(pid)
			}
			info = &pidInfo{uid: uid, name: name}
			st.procs[pid] = info
		}
		info.rss = rssPages
		seen[pid] = jiffies

		var delta uint64
		if prev, ok := st.prevPid[pid]; ok && jiffies >= prev {
			delta = jiffies - prev
		} else if !first && jiffies < clkTck {
			// short-lived process born and counted within this window
			delta = jiffies
		}
		st.prevPid[pid] = jiffies

		uid := info.uid
		agg := perUid[uid]
		if agg == nil {
			agg = &uidAgg{}
			perUid[uid] = agg
		}
		agg.jiffies += delta
		agg.memPages += rssPages
		agg.count++
		if delta > 0 {
			agg.top.add(ProcInfo{PID: pid, Name: truncate(info.name, 80), CPU: cpuPct(delta, dt, st.ncores), MemMB: pagesToMB(rssPages, st.pageSize)})
		}
	}
	// drop dead pids from caches
	if len(seen) < len(st.prevPid)/2 || len(st.procs) > len(seen)+500 {
		for pid := range st.procs {
			if _, ok := seen[pid]; !ok {
				delete(st.procs, pid)
				delete(st.prevPid, pid)
			}
		}
	}

	for uid, agg := range perUid {
		site := reg.SiteByUID(uid)
		var key string
		if site != nil {
			key = site.User
		} else {
			key = systemKey(reg, uid)
		}
		sl := &SiteLive{
			CPU:   cpuPct(agg.jiffies, dt, st.ncores),
			MemMB: pagesToMB(agg.memPages, st.pageSize),
			Procs: agg.count,
			Top:   agg.top.sorted(5),
		}
		if existing, ok := snap.Sites[key]; ok {
			existing.CPU += sl.CPU
			existing.MemMB += sl.MemMB
			existing.Procs += sl.Procs
			existing.Top = append(existing.Top, sl.Top...)
		} else {
			snap.Sites[key] = sl
		}
	}
	return snap
}

type uidAgg struct {
	jiffies  uint64
	memPages uint64
	count    int
	top      topList
}

func cpuPct(jiffies uint64, dt float64, ncores int) float64 {
	if dt <= 0 {
		return 0
	}
	return clampPct(float64(jiffies) / (clkTck * dt * float64(ncores)) * 100)
}

func pagesToMB(pages uint64, pageSize int) float64 {
	return float64(pages*uint64(pageSize)) / (1024 * 1024)
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// systemKey maps non-site UIDs to stable pseudo-site keys, aggregated later
// into the "System services" bucket by the engine.
func systemKey(reg *sites.Registry, uid int) string {
	if name := reg.UserNameForUID(uid); name != "" {
		return "_sys:" + name
	}
	return "_sys:uid" + strconv.Itoa(uid)
}
