// Package server serves the dashboard UI and its JSON API.
package server

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/REVENTOR-EU/SiteStats/internal/alerter"
	"github.com/REVENTOR-EU/SiteStats/internal/collect"
	"github.com/REVENTOR-EU/SiteStats/internal/config"
	"github.com/REVENTOR-EU/SiteStats/internal/engine"
	"github.com/REVENTOR-EU/SiteStats/internal/store"
)

//go:embed web
var webRoot embed.FS

var webFS, _ = fs.Sub(webRoot, "web")

const sessionCookie = "ws_session"

// Version is displayed in the UI footer.
const Version = "1.1.0"

type Server struct {
	cfg        *config.Config
	configPath string
	st         *store.Store
	col        *engine.Collector
	al         *alerter.Alerter
	attM       struct {
		sync.Mutex
		m map[string][]time.Time // ip -> failed login times
	}
	bots *botCache
}

func New(cfg *config.Config, st *store.Store, col *engine.Collector, al *alerter.Alerter, configPath string) *Server {
	s := &Server{cfg: cfg, configPath: configPath, st: st, col: col, al: al}
	s.attM.m = map[string][]time.Time{}
	s.bots = newBotCache()
	return s
}

// savePasswordHash persists the bcrypt hash to the config file and applies
// it to the live config.
func (s *Server) savePasswordHash(hash string) error {
	if s.configPath != "" {
		if cfg, err := config.Load(s.configPath); err == nil {
			cfg.PasswordHash = hash
			if err := config.Save(s.configPath, cfg); err != nil {
				return err
			}
		}
	}
	s.cfg.PasswordHash = hash
	return nil
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
	mux.HandleFunc("GET /api/setup-status", s.handleSetupStatus)
	mux.HandleFunc("POST /api/setup-password", s.handleSetupPassword)
	mux.HandleFunc("POST /api/password", s.auth(s.handlePasswordChange))
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.auth(s.handleLogout))
	mux.HandleFunc("GET /api/meta", s.auth(s.handleMeta))
	mux.HandleFunc("GET /api/overview", s.auth(s.handleOverview))
	mux.HandleFunc("GET /api/site/{user}", s.auth(s.handleSite))
	mux.HandleFunc("GET /api/site/{user}/live", s.auth(s.handleSiteLive))
	mux.HandleFunc("GET /api/bans", s.auth(s.handleBans))
	mux.HandleFunc("POST /api/ban", s.auth(s.handleBan))
	mux.HandleFunc("POST /api/unban", s.auth(s.handleUnban))
	mux.HandleFunc("GET /api/events", s.auth(s.handleEvents))
	mux.HandleFunc("GET /api/settings", s.auth(s.handleSettingsGet))
	mux.HandleFunc("POST /api/settings", s.auth(s.handleSettingsSave))
	mux.HandleFunc("POST /api/settings/test-email", s.auth(s.handleTestEmail))
	// API responses are live data: never let the browser cache them
	// (heuristic caching of GETs made the dashboard show stale stats).
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		mux.ServeHTTP(w, r)
	})
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
			writeErr(w, http.StatusServiceUnavailable, "password not configured; run: sitestats set-password")
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
	s.createSession(w, r)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// createSession issues a session token and sets the session cookie.
func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
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

// --- password management ----------------------------------------------------

// handleSetupStatus tells the UI whether a password has been configured yet
// (public: it only reveals whether first-time setup is pending).
func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]bool{"configured": s.cfg.PasswordHash != ""})
}

// handleSetupPassword sets the initial password; only works while no
// password is configured.
func (s *Server) handleSetupPassword(w http.ResponseWriter, r *http.Request) {
	if s.cfg.PasswordHash != "" {
		writeErr(w, http.StatusForbidden, "password already configured — sign in and use the profile menu to change it")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if len(req.Password) < 8 {
		writeErr(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, 500, "hash failed")
		return
	}
	if err := s.savePasswordHash(string(hash)); err != nil {
		writeErr(w, 500, "could not persist password: "+err.Error())
		return
	}
	log.Printf("initial password set via web interface")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handlePasswordChange lets a signed-in user set a new password.
func (s *Server) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if len(req.New) < 8 {
		writeErr(w, http.StatusBadRequest, "new password must be at least 8 characters")
		return
	}
	if s.cfg.PasswordHash != "" &&
		bcrypt.CompareHashAndPassword([]byte(s.cfg.PasswordHash), []byte(req.Current)) != nil {
		writeErr(w, http.StatusForbidden, "current password is wrong")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.New), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, 500, "hash failed")
		return
	}
	if err := s.savePasswordHash(string(hash)); err != nil {
		writeErr(w, 500, "could not persist password: "+err.Error())
		return
	}
	log.Printf("password changed via web interface")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// --- API handlers -----------------------------------------------------------

func parseRange(r *http.Request) int64 {
	switch r.URL.Query().Get("range") {
	case "15m":
		return 15 * 60
	case "1h":
		return 3600
	case "6h":
		return 6 * 3600
	case "24h":
		return 86400
	case "7d":
		return 7 * 86400
	case "30d":
		return 30 * 86400
	default:
		return 86400
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

// --- settings handlers -------------------------------------------------------

var validAlertMetrics = map[string]bool{
	"site.cpu": true, "site.mem": true, "site.mem_pct": true,
	"site.req": true, "site.err5": true,
	"system.cpu": true, "system.load1": true, "system.load5": true, "system.load15": true,
	"system.mem_pct": true,
}

var validAlertOps = map[string]bool{">": true, ">=": true, "<": true, "<=": true}

type ruleJSON struct {
	Name     string  `json:"name"`
	Metric   string  `json:"metric"`
	Site     string  `json:"site"`
	Op       string  `json:"op"`
	Value    float64 `json:"value"`
	For      string  `json:"for"`
	Cooldown string  `json:"cooldown"`
	Severity string  `json:"severity"`
}

type settingsJSON struct {
	Email struct {
		Enabled     bool     `json:"enabled"`
		SmtpHost    string   `json:"smtpHost"`
		Username    string   `json:"username"`
		Password    string   `json:"password"` // write-only; blank keeps the stored one
		HasPassword bool     `json:"hasPassword"`
		From        string   `json:"from"`
		To          []string `json:"to"`
	} `json:"email"`
	Webhook struct {
		Enabled bool   `json:"enabled"`
		URL     string `json:"url"`
	} `json:"webhook"`
	Rules []ruleJSON `json:"rules"`
}

func settingsFromConfig(ac config.AlertConfig) settingsJSON {
	var sj settingsJSON
	sj.Email.Enabled = ac.Email.Enabled
	sj.Email.SmtpHost = ac.Email.Host
	sj.Email.Username = ac.Email.Username
	sj.Email.HasPassword = ac.Email.Password != ""
	sj.Email.From = ac.Email.From
	sj.Email.To = ac.Email.To
	if sj.Email.To == nil {
		sj.Email.To = []string{}
	}
	sj.Webhook.Enabled = ac.Webhook.Enabled
	sj.Webhook.URL = ac.Webhook.URL
	sj.Rules = []ruleJSON{}
	for _, r := range ac.Rules {
		sj.Rules = append(sj.Rules, ruleJSON{
			Name: r.Name, Metric: r.Metric, Site: r.Site, Op: r.Op, Value: r.Value,
			For: r.For.String(), Cooldown: r.Cooldown.String(), Severity: r.Severity,
		})
	}
	return sj
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, settingsFromConfig(s.cfg.Alerts))
}

// settingsToAlerts converts and validates a settingsJSON payload into an
// AlertConfig; the SMTP password is taken from the live config when blank.
func settingsToAlerts(sj settingsJSON, prev config.AlertConfig) (config.AlertConfig, error) {
	ac := config.AlertConfig{
		Email: config.EmailConfig{
			Enabled:  sj.Email.Enabled,
			Host:     strings.TrimSpace(sj.Email.SmtpHost),
			Username: strings.TrimSpace(sj.Email.Username),
			Password: prev.Email.Password,
			From:     strings.TrimSpace(sj.Email.From),
			To:       []string{},
		},
		Webhook: config.WebhookConfig{Enabled: sj.Webhook.Enabled, URL: strings.TrimSpace(sj.Webhook.URL)},
	}
	// a non-empty submitted password replaces the stored one
	if p := sj.Email.Password; p != "" {
		ac.Email.Password = p
	}
	if ac.Email.Host != "" && !strings.Contains(ac.Email.Host, ":") {
		ac.Email.Host += ":587"
	}
	for _, to := range sj.Email.To {
		if to = strings.TrimSpace(to); to != "" {
			ac.Email.To = append(ac.Email.To, to)
		}
	}
	if ac.Email.Enabled && (len(ac.Email.To) == 0 || ac.Email.Host == "") {
		return ac, fmt.Errorf("email alerts need an SMTP host and at least one recipient")
	}
	if ac.Webhook.Enabled && ac.Webhook.URL == "" {
		return ac, fmt.Errorf("webhook alerts need a URL")
	}
	seen := map[string]bool{}
	for _, rj := range sj.Rules {
		name := strings.TrimSpace(rj.Name)
		if name == "" {
			return ac, fmt.Errorf("alert rule name must not be empty")
		}
		if seen[name] {
			return ac, fmt.Errorf("duplicate alert rule name %q", name)
		}
		seen[name] = true
		if !validAlertMetrics[rj.Metric] {
			return ac, fmt.Errorf("rule %q: unknown metric %q", name, rj.Metric)
		}
		if !validAlertOps[rj.Op] {
			return ac, fmt.Errorf("rule %q: invalid operator %q", name, rj.Op)
		}
		site := strings.TrimSpace(rj.Site)
		if site == "" {
			site = "*"
		}
		sev := rj.Severity
		if sev != "warning" && sev != "critical" {
			sev = "warning"
		}
		forD, err := config.ParseDurationOr(rj.For, 5*time.Minute)
		if err != nil {
			return ac, fmt.Errorf("rule %q: invalid for duration: %v", name, err)
		}
		cdD, err := config.ParseDurationOr(rj.Cooldown, time.Hour)
		if err != nil {
			return ac, fmt.Errorf("rule %q: invalid cooldown duration: %v", name, err)
		}
		ac.Rules = append(ac.Rules, config.AlertRule{
			Name: name, Metric: rj.Metric, Site: site, Op: rj.Op, Value: rj.Value,
			For: config.Duration{Duration: forD}, Cooldown: config.Duration{Duration: cdD}, Severity: sev,
		})
	}
	return ac, nil
}

// saveAlerts persists the alert config to disk and applies it live: the
// collector reads s.cfg.Alerts per notification, the alerter needs an
// explicit rule swap.
func (s *Server) saveAlerts(ac config.AlertConfig) error {
	if s.configPath != "" {
		disk, err := config.Load(s.configPath)
		if err != nil {
			// no config file (or unreadable): fall back to the live config
			copy := *s.cfg
			disk = &copy
		}
		disk.Alerts = ac
		if err := config.Save(s.configPath, disk); err != nil {
			return err
		}
	}
	s.cfg.Alerts = ac
	s.al.UpdateConfig(ac)
	return nil
}

func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	var sj settingsJSON
	if err := json.NewDecoder(r.Body).Decode(&sj); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	ac, err := settingsToAlerts(sj, s.cfg.Alerts)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.saveAlerts(ac); err != nil {
		writeErr(w, 500, "could not persist settings: "+err.Error())
		return
	}
	s.st.InsertEvent(time.Now().Unix(), "settings", "_system", "alert-settings-updated",
		fmt.Sprintf("alert settings updated (%d rules, email %s, webhook %s)",
			len(ac.Rules), onOff(ac.Email.Enabled), onOff(ac.Webhook.Enabled)))
	log.Printf("alert settings updated via web interface")
	writeJSON(w, 200, settingsFromConfig(ac))
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// handleTestEmail sends a probe mail using the submitted SMTP fields; an
// empty password means "use the saved one".
func (s *Server) handleTestEmail(w http.ResponseWriter, r *http.Request) {
	var sj settingsJSON
	if err := json.NewDecoder(r.Body).Decode(&sj); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	ac, err := settingsToAlerts(sj, s.cfg.Alerts)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if ac.Email.Host == "" || len(ac.Email.To) == 0 {
		writeErr(w, http.StatusBadRequest, "SMTP host and at least one recipient are required")
		return
	}
	ev := alerter.Event{
		Ts: time.Now().Unix(), Rule: "test", Site: "_system", Severity: "info",
		Message: "This is a test message from website-stats. If you read this, mail alerts are set up correctly.",
	}
	// the probe must send even when the email channel is switched off
	ec := ac.Email
	ec.Enabled = true
	if err := alerter.NotifyEmail(ec, ev); err != nil {
		writeErr(w, http.StatusBadGateway, "SMTP test failed: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
