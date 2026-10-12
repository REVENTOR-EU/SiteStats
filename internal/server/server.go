// Package server serves the dashboard UI and its JSON API.
package server

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"website-stats/internal/collect"
	"website-stats/internal/config"
	"website-stats/internal/engine"
	"website-stats/internal/store"
)

//go:embed web
var webRoot embed.FS

var webFS, _ = fs.Sub(webRoot, "web")

const sessionCookie = "ws_session"

// Version is displayed in the UI footer.
const Version = "1.0.0"

type Server struct {
	cfg  *config.Config
	st   *store.Store
	col  *engine.Collector
	attM struct {
		sync.Mutex
		m map[string][]time.Time // ip -> failed login times
	}
}

func New(cfg *config.Config, st *store.Store, col *engine.Collector) *Server {
	s := &Server{cfg: cfg, st: st, col: col}
	s.attM.m = map[string][]time.Time{}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// static assets are versioned via ?v=N in index.html: cache hard, and
	// bump N on every UI change. HTML itself must always revalidate.
	staticHandler := http.StripPrefix("/static/", http.FileServerFS(webFS))
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		staticHandler.ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		s.handleIndex(w, r)
	})
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.auth(s.handleLogout))
	mux.HandleFunc("GET /api/meta", s.auth(s.handleMeta))
	mux.HandleFunc("GET /api/overview", s.auth(s.handleOverview))
	mux.HandleFunc("GET /api/site/{user}", s.auth(s.handleSite))
	mux.HandleFunc("GET /api/events", s.auth(s.handleEvents))
	return mux
}

// --- helpers ----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// auth wraps API handlers with the session check.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.PasswordHash == "" {
			writeErr(w, http.StatusServiceUnavailable, "password not configured; run: website-stats set-password")
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err == nil && s.st.ValidSession(c.Value, int64(s.cfg.SessionTTL.Seconds()), time.Now().Unix()) {
			next(w, r)
			return
		}
		writeErr(w, http.StatusUnauthorized, "unauthorized")
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFileFS(w, r, webFS, "index.html")
}

// --- auth handlers ----------------------------------------------------------

func (s *Server) tooManyAttempts(ip string) bool {
	s.attM.Lock()
	defer s.attM.Unlock()
	now := time.Now()
	recent := s.attM.m[ip][:0]
	for _, t := range s.attM.m[ip] {
		if now.Sub(t) < 5*time.Minute {
			recent = append(recent, t)
		}
	}
	s.attM.m[ip] = recent
	return len(recent) >= 8
}

func (s *Server) recordAttempt(ip string) {
	s.attM.Lock()
	defer s.attM.Unlock()
	s.attM.m[ip] = append(s.attM.m[ip], time.Now())
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip, _, _ := strings.Cut(r.RemoteAddr, ":")
	if s.tooManyAttempts(ip) {
		writeErr(w, http.StatusTooManyRequests, "too many login attempts; wait a few minutes")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if s.cfg.PasswordHash == "" {
		writeErr(w, http.StatusServiceUnavailable, "password not configured; run: website-stats set-password")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(s.cfg.PasswordHash), []byte(req.Password)) != nil {
		s.recordAttempt(ip)
		writeErr(w, http.StatusUnauthorized, "wrong password")
		return
	}
	token := make([]byte, 32)
	rand.Read(token)
	tokHex := hex.EncodeToString(token)
	s.st.CreateSession(tokHex, time.Now().Unix())

	secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    tokHex,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(s.cfg.SessionTTL.Seconds()),
	})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.st.DeleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// --- API handlers -----------------------------------------------------------

func parseRange(r *http.Request) int64 {
	switch r.URL.Query().Get("range") {
	case "1h":
		return 3600
	case "6h":
		return 6 * 3600
	case "7d":
		return 7 * 86400
	case "30d":
		return 30 * 86400
	default:
		return 24 * 86400
	}
}

func bucketFor(rangeSec int64) int64 {
	b := rangeSec / 720
	if b < 60 {
		b = 60
	}
	// align buckets to whole minutes
	return b / 60 * 60
}

type siteJSON struct {
	Name   string             `json:"name"`
	Domain string             `json:"domain"`
	CPU    float64            `json:"cpu"`
	Mem    float64            `json:"mem"`
	Req    int64              `json:"req"`
	R4     int64              `json:"r4"`
	R5     int64              `json:"r5"`
	Procs  int                `json:"procs"`
	Fpm    *collect.FpmStatus `json:"fpm,omitempty"`
	Series [][2]float64       `json:"series"`
}

type overviewJSON struct {
	Now          map[string]float64 `json:"now"`
	SystemSeries [][2]float64       `json:"systemSeries"`
	SystemMem    [][2]float64       `json:"systemMem"`
	Sites        []siteJSON         `json:"sites"`
	Events       []store.Event      `json:"events"`
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	rangeSec := parseRange(r)
	bucket := bucketFor(rangeSec)
	now := time.Now().Unix()
	since := now - rangeSec

	pts, err := s.st.QueryMinuteSeries(since, bucket)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	sysSeries := [][2]float64{}
	sysMem := [][2]float64{}
	perSite := map[string][][2]float64{}
	for _, p := range pts {
		if p.Site == "_system" {
			sysSeries = append(sysSeries, [2]float64{float64(p.Ts), p.CPU})
			sysMem = append(sysMem, [2]float64{float64(p.Ts), p.Mem})
			continue
		}
		perSite[p.Site] = append(perSite[p.Site], [2]float64{float64(p.Ts), p.CPU})
	}

	latest := map[string]store.MinutePoint{}
	if lp, err := s.st.QueryLatestMinute(); err == nil {
		for _, p := range lp {
			latest[p.Site] = p
		}
	}

	resp := overviewJSON{
		SystemSeries: sysSeries,
		SystemMem:    sysMem,
		Sites:        []siteJSON{},
		Events:       []store.Event{},
	}
	if snap := s.col.Latest(); snap != nil {
		resp.Now = map[string]float64{
			"ts":        float64(snap.Ts),
			"cpu":       snap.System.CPU,
			"memUsed":   snap.System.MemUsedMB,
			"memTotal":  snap.System.MemTotalMB,
			"load1":     snap.System.Load1,
			"netRx":     snap.System.NetRx,
			"netTx":     snap.System.NetTx,
			"diskUsed":  snap.System.DiskUsedGB,
			"diskTotal": snap.System.DiskTotalGB,
		}
	}
	if _, ok := resp.Now["cpu"]; !ok {
		if p, ok := latest["_system"]; ok {
			resp.Now = map[string]float64{"ts": float64(p.Ts), "cpu": p.CPU, "memUsed": p.Mem}
		}
	}

	for _, site := range s.col.Registry().Sites() {
		sj := siteJSON{Name: site.User, Domain: site.Domain}
		if p, ok := latest[site.User]; ok {
			sj.CPU, sj.Mem, sj.Req, sj.R4, sj.R5, sj.Procs = p.CPU, p.Mem, p.Req, p.R4, p.R5, int(p.Procs)
		}
		if f := s.col.FpmFor(site.User); f != nil {
			sj.Fpm = f
		}
		sj.Series = perSite[site.User]
		resp.Sites = append(resp.Sites, sj)
	}
	sort.Slice(resp.Sites, func(i, j int) bool { return resp.Sites[i].CPU > resp.Sites[j].CPU })

	// "System services" bucket: aggregated non-site users (mysql, root, ...)
	if _, ok := perSite["_sys"]; ok {
		sj := siteJSON{Name: "_sys", Domain: "System services"}
		if p, ok := latest["_sys"]; ok {
			sj.CPU, sj.Mem, sj.Procs = p.CPU, p.Mem, int(p.Procs)
		}
		sj.Series = perSite["_sys"]
		resp.Sites = append(resp.Sites, sj)
	}

	if ev, err := s.st.QueryEvents(5, ""); err == nil {
		resp.Events = ev
	}
	writeJSON(w, 200, resp)
}

type siteDetailJSON struct {
	Name    string                  `json:"name"`
	Domain  string                  `json:"domain"`
	Series  map[string][][2]float64 `json:"series"`
	Disk    [][2]float64            `json:"disk"`
	TopUrls []store.TopRow          `json:"topUrls"`
	TopIps  []store.TopRow          `json:"topIps"`
	Procs   []collect.ProcInfo      `json:"procs"`
	Fpm     *collect.FpmStatus      `json:"fpm,omitempty"`
	Events  []store.Event           `json:"events"`
	Latest  map[string]float64      `json:"latest"`
}

func (s *Server) handleSite(w http.ResponseWriter, r *http.Request) {
	user := r.PathValue("user")
	site := s.col.Registry().Get(user)
	if site == nil {
		writeErr(w, 404, "unknown site")
		return
	}
	rangeSec := parseRange(r)
	bucket := bucketFor(rangeSec)
	now := time.Now().Unix()
	since := now - rangeSec

	det := siteDetailJSON{
		Name:    site.User,
		Domain:  site.Domain,
		Series:  map[string][][2]float64{},
		TopUrls: []store.TopRow{},
		TopIps:  []store.TopRow{},
		Procs:   []collect.ProcInfo{},
		Events:  []store.Event{},
		Latest:  map[string]float64{},
	}

	pts, err := s.st.QueryMinuteSeries(since, bucket)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for _, p := range pts {
		if p.Site != user {
			continue
		}
		add := func(k string, v float64) {
			det.Series[k] = append(det.Series[k], [2]float64{float64(p.Ts), v})
		}
		add("cpu", p.CPU)
		add("mem", p.Mem)
		add("req", float64(p.Req))
		add("bytes", float64(p.Bytes))
		add("r4", float64(p.R4))
		add("r5", float64(p.R5))
		add("fpmActive", float64(p.FpmActive))
		add("fpmReq", float64(p.FpmReq))
	}

	if dp, err := s.st.QueryDisk(since, user); err == nil {
		for _, d := range dp {
			det.Disk = append(det.Disk, [2]float64{float64(d.Ts), float64(d.Bytes)})
		}
	}
	if tp, err := s.st.QueryTop("urlstats", user, now-86400, 15); err == nil {
		det.TopUrls = tp
	}
	if tp, err := s.st.QueryTop("ipstats", user, now-86400, 15); err == nil {
		det.TopIps = tp
	}
	if ev, err := s.st.QueryEvents(20, user); err == nil {
		det.Events = ev
	}
	if snap := s.col.Latest(); snap != nil {
		if sl, ok := snap.Sites[user]; ok {
			det.Procs = sl.Top
			det.Fpm = sl.Fpm
			det.Latest["cpu"] = sl.CPU
			det.Latest["mem"] = sl.MemMB
			det.Latest["procs"] = float64(sl.Procs)
		}
	}
	if lp, err := s.st.QueryLatestMinute(); err == nil {
		for _, p := range lp {
			if p.Site == user {
				det.Latest["req"] = float64(p.Req)
				det.Latest["r5"] = float64(p.R5)
				det.Latest["r4"] = float64(p.R4)
			}
		}
	}
	writeJSON(w, 200, det)
}

type metaJSON struct {
	Version string             `json:"version"`
	Sites   []metaSite         `json:"sites"`
	System  map[string]float64 `json:"system"`
}

type metaSite struct {
	Name    string `json:"name"`
	Domain  string `json:"domain"`
	LogPath string `json:"logPath"`
	Pools   int    `json:"pools"`
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	m := metaJSON{Version: Version, Sites: []metaSite{}}
	for _, site := range s.col.Registry().Sites() {
		m.Sites = append(m.Sites, metaSite{Name: site.User, Domain: site.Domain, LogPath: site.LogPath, Pools: len(site.Pools)})
	}
	if snap := s.col.Latest(); snap != nil {
		m.System = map[string]float64{
			"cpu": snap.System.CPU, "memUsed": snap.System.MemUsedMB, "memTotal": snap.System.MemTotalMB,
		}
	}
	writeJSON(w, 200, m)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 500 {
		limit = l
	}
	ev, err := s.st.QueryEvents(limit, r.URL.Query().Get("site"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, ev)
}
