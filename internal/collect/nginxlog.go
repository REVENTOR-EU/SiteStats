package collect

import (
	"bytes"
	"context"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"website-stats/internal/sites"
)

// TrafficMinute aggregates one site's HTTP traffic for one minute.
type TrafficMinute struct {
	Req   int64
	Bytes int64
	R4xx  int64
	R5xx  int64
}

// TopEntry is one row of a "top URLs/IPs" window.
type TopEntry struct {
	Value string
	Count int64
	Bytes int64
}

// TopWindow is the drained result of one aggregation window.
type TopWindow struct {
	Start int64
	URLs  map[string][]TopEntry // site -> entries
	IPs   map[string][]TopEntry
}

// LogTailer follows every site's nginx access log, aggregates traffic per
// minute and top URLs/IPs per window. It handles log rotation (both
// new-file and truncate styles).
type LogTailer struct {
	mu      sync.Mutex
	reg     *sites.Registry
	files   map[string]*tailState
	unknown map[string]bool // log paths not mapped to a site (report once)

	minute map[string]*TrafficMinute

	winDur     time.Duration
	winStart   time.Time
	urls       map[string]map[string]*TopEntry
	ips        map[string]map[string]*TopEntry
	topKeep    int
	hasWindow  bool
	lastWindow TopWindow
}

type tailState struct {
	f      *os.File
	ino    uint64
	offset int64
	carry  []byte
}

func NewLogTailer(reg *sites.Registry, window time.Duration, topKeep int) *LogTailer {
	return &LogTailer{
		reg:      reg,
		files:    map[string]*tailState{},
		unknown:  map[string]bool{},
		minute:   map[string]*TrafficMinute{},
		winDur:   window,
		winStart: time.Now().Truncate(window),
		urls:     map[string]map[string]*TopEntry{},
		ips:      map[string]map[string]*TopEntry{},
		topKeep:  topKeep,
	}
}

// SetRegistry swaps in a refreshed site registry; new log paths are picked up
// on the next poll, removed ones are dropped.
func (t *LogTailer) SetRegistry(reg *sites.Registry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reg = reg
	for path, st := range t.files {
		if reg.ByLogPath(path) == nil {
			st.f.Close()
			delete(t.files, path)
		}
	}
}

// Run polls all known log files once per second until ctx is cancelled.
func (t *LogTailer) Run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			t.poll()
		}
	}
}

func (t *LogTailer) poll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.reg.Sites() {
		if s.LogPath == "" {
			continue
		}
		st, ok := t.files[s.LogPath]
		if !ok {
			f, ino, err := openLog(s.LogPath)
			if err != nil {
				continue // may not exist yet; retry next poll
			}
			t.files[s.LogPath] = &tailState{f: f, ino: ino}
			continue // start at EOF; no history backfill
		}
		t.tailFile(s, st)
	}
	t.advanceWindows()
}

func (t *LogTailer) tailFile(site *sites.Site, st *tailState) {
	info, err := st.f.Stat()
	if err != nil {
		return
	}
	if fileIno(info) != st.ino || info.Size() < st.offset {
		// rotated or truncated: reopen and start over
		st.f.Close()
		f, ino, err := openLog(site.LogPath)
		if err != nil {
			delete(t.files, site.LogPath)
			return
		}
		*st = tailState{f: f, ino: ino}
	}
	if _, err := st.f.Seek(st.offset, io.SeekStart); err != nil {
		return
	}
	data, err := io.ReadAll(io.LimitReader(st.f, 8<<20)) // 8MB safety cap per poll
	if err != nil {
		return
	}
	st.offset += int64(len(data))

	buf := append(st.carry, data...)
	lines := bytes.Split(buf, []byte{'\n'})
	st.carry = lines[len(lines)-1]
	for _, line := range lines[:len(lines)-1] {
		t.consume(site, string(line))
	}
}

func (t *LogTailer) consume(site *sites.Site, line string) {
	if line == "" {
		return
	}
	ip, _, reqPath, status, nbytes, ok := parseAccessLine(line)
	if !ok {
		t.unknown[site.User] = true // malformed line; counted once per site
		return
	}
	agg := t.minute[site.User]
	if agg == nil {
		agg = &TrafficMinute{}
		t.minute[site.User] = agg
	}
	agg.Req++
	agg.Bytes += nbytes
	switch {
	case status >= 500:
		agg.R5xx++
	case status >= 400:
		agg.R4xx++
	}

	putTop(t.urls, site.User, reqPath, nbytes)
	putTop(t.ips, site.User, ip, nbytes)
}

// maxTopEntries bounds the per-site top maps: a flood of unique paths or IPs
// must not grow memory without limit. Entries beyond the cap are still
// counted in the traffic totals, just not tracked individually.
const maxTopEntries = 5000

func putTop(m map[string]map[string]*TopEntry, site, val string, nbytes int64) {
	siteMap := m[site]
	if siteMap == nil {
		siteMap = map[string]*TopEntry{}
		m[site] = siteMap
	}
	if len(siteMap) >= maxTopEntries {
		if _, ok := siteMap[val]; !ok {
			return
		}
	}
	e := siteMap[val]
	if e == nil {
		e = &TopEntry{Value: val}
		siteMap[val] = e
	}
	e.Count++
	e.Bytes += nbytes
}

// advanceWindows closes the top-URL/IP window when due.
func (t *LogTailer) advanceWindows() {
	now := time.Now()
	if !t.winStart.Add(t.winDur).After(now) {
		t.winStart = t.winStart.Add(t.winDur)
		if t.winStart.Add(t.winDur).Before(now) {
			t.winStart = now.Truncate(t.winDur) // fell far behind: resync
		}
		t.drainTopLocked()
	}
}

func (t *LogTailer) drainTopLocked() {
	t.lastWindow = TopWindow{
		Start: t.winStart.Unix() - int64(t.winDur/time.Second),
		URLs:  map[string][]TopEntry{},
		IPs:   map[string][]TopEntry{},
	}
	for site, m := range t.urls {
		t.lastWindow.URLs[site] = topN(m, t.topKeep)
		delete(t.urls, site)
	}
	for site, m := range t.ips {
		t.lastWindow.IPs[site] = topN(m, t.topKeep)
		delete(t.ips, site)
	}
	t.hasWindow = true
}

func topN(m map[string]*TopEntry, n int) []TopEntry {
	out := make([]TopEntry, 0, len(m))
	for _, e := range m {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// DrainMinute returns and resets the per-site traffic counters aggregated so
// far, tagged with the unix minute ts passed in.
func (t *LogTailer) DrainMinute(ts int64) map[string]*TrafficMinute {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := t.minute
	t.minute = map[string]*TrafficMinute{}
	return out
}

// DrainTop returns the last completed top-URL/IP window, if any.
func (t *LogTailer) DrainTop() (TopWindow, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.hasWindow {
		return TopWindow{}, false
	}
	t.hasWindow = false
	return t.lastWindow, true
}
