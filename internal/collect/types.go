package collect

// ProcInfo is one process entry shown in a site's "top processes" list.
type ProcInfo struct {
	PID   int     `json:"pid"`
	Name  string  `json:"name"`
	CPU   float64 `json:"cpu"`   // % of total server CPU in last interval
	MemMB float64 `json:"memMb"` // resident
}

// FpmStatus is the parsed JSON output of a PHP-FPM pool status page.
type FpmStatus struct {
	Pool           string `json:"pool"`
	Active         int    `json:"active"`
	Idle           int    `json:"idle"`
	TotalProcs     int    `json:"totalProcs"`
	ListenQueue    int    `json:"listenQueue"`
	MaxListenQueue int    `json:"maxListenQueue"`
	AcceptedConn   int64  `json:"acceptedConn"`
	MaxChildren    int64  `json:"maxChildrenReached"`
	SlowRequests   int64  `json:"slowRequests"`
}

// SiteLive is the current reading for one site, refreshed every scan interval.
type SiteLive struct {
	CPU   float64    `json:"cpu"` // % of total server CPU capacity
	MemMB float64    `json:"memMb"`
	Procs int        `json:"procs"`
	Top   []ProcInfo `json:"top,omitempty"`
	Fpm   *FpmStatus `json:"fpm,omitempty"` // summed over the site's pools
}

// SystemLive is the current whole-host reading.
type SystemLive struct {
	CPU         float64 `json:"cpu"` // % of total capacity
	MemUsedMB   float64 `json:"memUsedMb"`
	MemTotalMB  float64 `json:"memTotalMb"`
	Load1       float64 `json:"load1"`
	Load5       float64 `json:"load5"`
	Load15      float64 `json:"load15"`
	NetRx       float64 `json:"netRx"` // bytes/sec
	NetTx       float64 `json:"netTx"`
	DiskUsedGB  float64 `json:"diskUsedGb"`
	DiskTotalGB float64 `json:"diskTotalGb"`
}

// Snapshot is one full scan of the host.
type Snapshot struct {
	Ts     int64                `json:"ts"`
	System SystemLive           `json:"system"`
	Sites  map[string]*SiteLive `json:"sites"`
}

// MinuteRow is the per-minute rollup persisted for one site
// ("_system" is the pseudo-site holding whole-host averages).
type MinuteRow struct {
	Ts        int64
	Site      string
	CPU       float64 // average % of total over the minute
	MemMB     float64 // last value in minute
	Procs     int
	Req       int64 // requests in minute
	Bytes     int64
	R4xx      int64
	R5xx      int64
	FpmActive int
	FpmIdle   int
	FpmQueue  int
	FpmReq    int64 // delta of accepted conns in minute
	// load averages, system pseudo-row only
	Load1  float64
	Load5  float64
	Load15 float64
}
