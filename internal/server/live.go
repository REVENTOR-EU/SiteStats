package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"

	"github.com/REVENTOR-EU/SiteStats/internal/geo"
)

// liveHitJSON is one entry of the site's live request log.
type liveHitJSON struct {
	Ts      int64      `json:"ts"`
	IP      string     `json:"ip"`
	Method  string     `json:"method"`
	Path    string     `json:"path"`
	Status  int        `json:"status"`
	Bytes   int64      `json:"bytes"`
	Bot     BotVerdict `json:"bot"`
	Country string     `json:"country"`
	City    string     `json:"city"`
	ASNOrg  string     `json:"asnorg"`
}

func (s *Server) handleSiteLive(w http.ResponseWriter, r *http.Request) {
	user := r.PathValue("user")
	hits := s.col.LiveHits(user)
	// newest first; classify each unique IP once per response
	out := make([]liveHitJSON, 0, len(hits))
	type ipInfo struct {
		bot BotVerdict
		g   geo.Lookup
	}
	cache := map[string]ipInfo{}
	for i := len(hits) - 1; i >= 0; i-- {
		h := hits[i]
		info, ok := cache[h.IP]
		if !ok {
			info.bot = s.bots.Classify(h.IP, h.UA)
			if s.geo != nil {
				info.g = s.geo.Lookup(net.ParseIP(h.IP))
			}
			cache[h.IP] = info
		}
		out = append(out, liveHitJSON{
			Ts: h.Ts, IP: h.IP, Method: h.Method, Path: h.Path,
			Status: h.Status, Bytes: h.Bytes, Bot: info.bot,
			Country: info.g.Country, City: info.g.City, ASNOrg: info.g.ASNOrg,
		})
	}
	writeJSON(w, 200, map[string]any{"hits": out})
}

// banIP inserts (or removes) a ufw deny rule for one IP. ufw rules persist
// across reboots; the comment marks rules created from the dashboard.
func (s *Server) ufw(args ...string) error {
	path := "/usr/sbin/ufw"
	cmd := exec.Command(path, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("ufw %s: %s", strings.Join(args, " "), msg)
	}
	return nil
}

// validTarget accepts a single IP or a CIDR range (ufw handles both).
func validTarget(s string) bool {
	if strings.Contains(s, "/") {
		_, _, err := net.ParseCIDR(s)
		return err == nil
	}
	return net.ParseIP(s) != nil
}

func (s *Server) handleBan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IP    string `json:"ip"`
		Unban bool   `json:"unban"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	req.IP = strings.TrimSpace(req.IP)
	if !validTarget(req.IP) {
		writeErr(w, 400, "invalid IP address")
		return
	}
	var err error
	if req.Unban {
		err = s.ufw("--delete", "deny", "from", req.IP, "comment", "sitestats-manual")
	} else {
		err = s.ufw("insert", "1", "deny", "from", req.IP, "comment", "sitestats-manual")
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "true"})
}

func (s *Server) handleUnban(w http.ResponseWriter, r *http.Request) {
	s.handleBan(w, r) // same payload shape, Unban flag distinguishes
}

// handleBans returns every IP (v4+v6) currently denied by ufw — whether the rule
// was created from the dashboard, by fail2ban, or by hand.
func (s *Server) handleBans(w http.ResponseWriter, r *http.Request) {
	out, err := exec.Command("/usr/sbin/ufw", "status").CombinedOutput()
	if err != nil {
		writeErr(w, 500, "ufw status: "+err.Error())
		return
	}
	banned := []string{}
	for _, ln := range strings.Split(string(out), "\n") {
		ln = strings.TrimSpace(ln)
		fields := strings.Fields(ln)
		// "Anywhere DENY IN 1.2.3.4" / "1.2.3.4 LIMIT IN Anywhere" etc.;
		// match any field that parses as an IPv4 and pair it with the action
		action := ""
		for _, f := range fields {
			switch strings.ToLower(f) {
			case "deny", "reject":
				action = "deny"
			}
		}
		for _, f := range fields {
			if validTarget(f) && action == "deny" {
				banned = append(banned, f)
			}
		}
	}
	writeJSON(w, 200, map[string]any{"banned": banned})
}
