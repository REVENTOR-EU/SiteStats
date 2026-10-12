// Package sites discovers the websites served by a CloudPanel instance by
// correlating nginx vhosts, PHP-FPM pools and local user accounts.
package sites

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// PoolAddr is one PHP-FPM pool listen endpoint of a site.
type PoolAddr struct {
	Addr       string `json:"addr"`       // host:port
	StatusPath string `json:"statusPath"` // pm.status_path
}

// Site is one website: a Linux user owning PHP-FPM workers, an nginx vhost
// and a per-vhost access log.
type Site struct {
	User    string     `json:"user"` // stable key (linux username)
	UID     int        `json:"uid"`
	Domain  string     `json:"domain"` // primary server_name
	Home    string     `json:"home"`
	LogPath string     `json:"logPath"` // access log; empty if not found
	Pools   []PoolAddr `json:"pools"`   // FastCGI endpoints
}

// Registry maps users, log paths and pool addresses to sites.
type Registry struct {
	mu        sync.RWMutex
	byUser    map[string]*Site
	byLogPath map[string]*Site
	byPool    map[string]*Site // "host:port" -> site
	uidNames  map[int]string   // every local uid -> username (for system buckets)
}

func (r *Registry) Sites() []*Site {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Site, 0, len(r.byUser))
	for _, s := range r.byUser {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].User < out[j].User })
	return out
}

func (r *Registry) Get(user string) *Site {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byUser[user]
}

func (r *Registry) ByLogPath(path string) *Site {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byLogPath[path]
}

func (r *Registry) SiteByUID(uid int) *Site {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.byUser {
		if s.UID == uid {
			return s
		}
	}
	return nil
}

// UserNameForUID resolves any local uid to its username ("" if unknown).
func (r *Registry) UserNameForUID(uid int) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.uidNames[uid]
}

func (r *Registry) snapshot() *Registry {
	n := &Registry{
		byUser:    make(map[string]*Site, len(r.byUser)),
		byLogPath: make(map[string]*Site, len(r.byLogPath)),
		byPool:    make(map[string]*Site, len(r.byPool)),
	}
	for k, v := range r.byUser {
		n.byUser[k] = v
	}
	for k, v := range r.byLogPath {
		n.byLogPath[k] = v
	}
	for k, v := range r.byPool {
		n.byPool[k] = v
	}
	return n
}

// SetSites installs a static site list (demo mode / tests).
func (r *Registry) SetSites(list []*Site) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byUser = map[string]*Site{}
	r.byLogPath = map[string]*Site{}
	r.byPool = map[string]*Site{}
	r.uidNames = map[int]string{}
	for _, s := range list {
		r.byUser[s.User] = s
		r.uidNames[s.UID] = s.User
		if s.Domain == "" {
			s.Domain = s.User
		}
	}
}

// NewEmpty returns an empty registry (used when discovery fails at startup).
func NewEmpty() *Registry {
	return &Registry{
		byUser:    map[string]*Site{},
		byLogPath: map[string]*Site{},
		byPool:    map[string]*Site{},
		uidNames:  map[int]string{},
	}
}

// Discover builds a fresh registry from nginx vhost configs, PHP-FPM pool
// configs and /etc/passwd. Sites are users with a vhost or a pool; system
// accounts without either (clp, beszel, agent, ...) are ignored.
func Discover(nginxSitesDir, fpmPoolGlob string) (*Registry, error) {
	users, err := readHomeUsers()
	if err != nil {
		return nil, err
	}
	byUser := map[string]*Site{}
	get := func(name string) *Site {
		s := byUser[name]
		if s == nil {
			u, ok := users[name]
			if !ok {
				return nil
			}
			s = &Site{User: name, UID: u.uid, Home: u.home}
			byUser[name] = s
		}
		return s
	}

	vhosts := parseVhosts(nginxSitesDir)
	for _, v := range vhosts {
		if _, ok := users[v.user]; !ok {
			continue // vhost with no matching local user: skip
		}
		s := get(v.user)
		if v.domain != "" {
			s.Domain = v.domain
		}
		if v.logPath != "" {
			s.LogPath = v.logPath
		}
	}

	for _, p := range globOrEmpty(fpmPoolGlob) {
		pool, err := parsePool(p)
		if err != nil || pool.user == "" || pool.listen == "" {
			continue
		}
		if !strings.Contains(pool.listen, ":") {
			continue // unix socket: skip (tcp-only FastCGI client)
		}
		s := get(pool.user)
		if s == nil { // pool user with no /home entry (e.g. www-data defaults)
			continue
		}
		if s.Domain == "" {
			s.Domain = pool.name
		}
		s.Pools = append(s.Pools, PoolAddr{Addr: pool.listen, StatusPath: pool.status})
	}

	r := &Registry{
		byUser:    byUser,
		byLogPath: map[string]*Site{},
		byPool:    map[string]*Site{},
		uidNames:  map[int]string{},
	}
	if all, err := readAllUsers(); err == nil {
		r.uidNames = all
	}
	for _, s := range byUser {
		if s.Domain == "" {
			s.Domain = s.User
		}
		if s.LogPath != "" {
			r.byLogPath[s.LogPath] = s
		}
		for _, p := range s.Pools {
			r.byPool[p.Addr] = s
		}
	}
	return r, nil
}

type homeUser struct {
	uid  int
	home string
}

// readAllUsers returns every local uid -> username, used to label non-site
// processes (mysql, www-data, root, ...) in the "System services" bucket.
func readAllUsers() (map[int]string, error) {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[int]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Split(sc.Text(), ":")
		if len(parts) < 3 {
			continue
		}
		uid, err := strconv.Atoi(parts[2])
		if err != nil {
			continue
		}
		out[uid] = parts[0]
	}
	return out, sc.Err()
}

func readHomeUsers() (map[string]homeUser, error) {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]homeUser{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Split(sc.Text(), ":")
		if len(parts) < 6 {
			continue
		}
		uid, err := strconv.Atoi(parts[2])
		if err != nil || uid < 1000 || uid == 65534 {
			continue // system accounts and nobody
		}
		out[parts[0]] = homeUser{uid: uid, home: parts[5]}
	}
	return out, sc.Err()
}

type vhost struct {
	user    string // best-effort: derived from the access log path /home/<user>/...
	domain  string
	logPath string
}

// parseVhosts extracts server_name and access_log from each enabled vhost.
// CloudPanel writes one file per site with a per-site access log at
// /home/<user>/logs/nginx/access.log; the user is derived from that path.
func parseVhosts(dir string) []vhost {
	var out []vhost
	for _, path := range globOrEmpty(filepath.Join(dir, "*.conf")) {
		v := parseVhostFile(path)
		if v.domain != "" || v.logPath != "" {
			out = append(out, v)
		}
	}
	return out
}

func parseVhostFile(path string) vhost {
	f, err := os.Open(path)
	if err != nil {
		return vhost{}
	}
	defer f.Close()

	// CloudPanel vhosts contain several server blocks (redirect, real site,
	// :8080 staging). The site identity comes from the block that defines an
	// access_log; fall back to the first server_name seen.
	var best, cur vhost
	firstDomain := ""
	depth := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "server ") || strings.HasPrefix(line, "server	") || line == "server" || strings.HasPrefix(line, "server{") {
			if strings.HasSuffix(line, "{") {
				if depth == 0 {
					cur = vhost{}
				}
				depth++
				continue
			}
		}
		if line == "{" && depth == 0 {
			depth++
			continue
		}
		if strings.HasPrefix(line, "server_name") && depth >= 1 && cur.domain == "" {
			if fields := strings.Fields(strings.TrimSuffix(strings.TrimPrefix(line, "server_name"), ";")); len(fields) > 0 {
				cur.domain = fields[0]
				if firstDomain == "" {
					firstDomain = fields[0]
				}
			}
			continue
		}
		if strings.HasPrefix(line, "access_log") {
			rest := strings.TrimSuffix(strings.TrimPrefix(line, "access_log"), ";")
			fields := strings.Fields(rest)
			if len(fields) > 0 && fields[0] != "off" && cur.logPath == "" {
				cur.logPath = fields[0]
			}
			continue
		}
		if strings.HasSuffix(line, "{") {
			depth++
			continue
		}
		if strings.HasPrefix(line, "}") {
			depth--
			if depth == 0 {
				if cur.logPath != "" && best.logPath == "" {
					best = cur
				}
				cur = vhost{}
			}
		}
	}
	if best.domain == "" {
		best.domain = firstDomain
	}
	if best.logPath != "" {
		rel, err := filepath.Rel("/home", filepath.Dir(filepath.Dir(filepath.Dir(best.logPath))))
		if err == nil && !strings.Contains(rel, "..") && rel != "." {
			best.user = filepath.ToSlash(rel)
		}
	}
	return best
}

type poolConf struct {
	name   string
	user   string
	listen string
	status string
}

func parsePool(path string) (poolConf, error) {
	f, err := os.Open(path)
	if err != nil {
		return poolConf{}, err
	}
	defer f.Close()
	var p poolConf
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			p.name = line[1 : len(line)-1]
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "user":
			p.user = v
		case "listen":
			p.listen = v
		case "pm.status_path":
			p.status = v
		}
	}
	return p, sc.Err()
}

func globOrEmpty(pattern string) []string {
	m, _ := filepath.Glob(pattern)
	sort.Strings(m)
	return m
}
