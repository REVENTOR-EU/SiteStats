// Command website-stats is a per-site monitoring dashboard for CloudPanel
// servers: CPU/RAM per site (process attribution by Linux user), HTTP traffic
// per vhost, PHP-FPM pool status, disk usage and threshold alerts.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"

	"github.com/REVENTOR-EU/SiteStats/internal/alerter"
	"github.com/REVENTOR-EU/SiteStats/internal/config"
	"github.com/REVENTOR-EU/SiteStats/internal/engine"
	"github.com/REVENTOR-EU/SiteStats/internal/server"
	"github.com/REVENTOR-EU/SiteStats/internal/sites"
	"github.com/REVENTOR-EU/SiteStats/internal/store"
)

var configPaths = []string{
	"/etc/website-stats/config.yml",
	"/opt/website-stats/config.yml",
	"config.yml",
}

func findConfig(flagPath string) string {
	if flagPath != "" {
		return flagPath
	}
	for _, p := range configPaths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return configPaths[0]
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	flagConf := flag.String("config", "", "path to config.yml")
	flag.Parse()

	cmd := "serve"
	if args := flag.Args(); len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "serve":
		runServe(findConfig(*flagConf), false)
	case "demo":
		runServe(findConfig(*flagConf), true)
	case "set-password":
		runSetPassword(findConfig(*flagConf))
	case "inspect":
		runInspect(findConfig(*flagConf))
	case "write-nginx-map":
		runWriteNginxMap(findConfig(*flagConf))
	default:
		fmt.Fprintf(os.Stderr, "usage: website-stats [-config path] [serve|demo|set-password|inspect|write-nginx-map]\n")
		os.Exit(2)
	}
}

func runServe(path string, demo bool) {
	cfg, err := config.Load(path)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	al := alerter.New(cfg.Alerts)
	col := engine.NewCollector(cfg, st, al)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if demo {
		go col.RunDemo(ctx)
	} else {
		go col.Run(ctx)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(cfg, st, col, al, path).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("website-stats %s listening on %s (config %s)", server.Version, cfg.Listen, path)
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	select {
	case err := <-errCh:
		log.Fatalf("http: %v", err)
	case <-ctx.Done():
	}
	// graceful: stop listeners and wait for in-flight requests
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
}

func runSetPassword(path string) {
	cfg, err := config.Load(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Fatalf("load config: %v", err)
		}
		cfg = &config.Config{}
		cfg.ApplyDefaults()
	}
	var pw string
	if rest := flag.Args()[1:]; len(rest) > 0 {
		pw = rest[0]
	} else {
		fmt.Print("Enter new password: ")
		b1, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Print("Confirm password: ")
		b2, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			log.Fatal(err)
		}
		if string(b1) != string(b2) {
			log.Fatal("passwords do not match")
		}
		pw = string(b1)
	}
	if len(pw) < 8 {
		log.Fatal("password must be at least 8 characters")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		log.Fatal(err)
	}
	cfg.PasswordHash = string(h)
	out, err := yaml.Marshal(cfg)
	if err != nil {
		log.Fatal(err)
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			log.Fatal(err)
		}
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		log.Fatalf("write config: %v", err)
	}
	fmt.Printf("password hash written to %s\n", path)
}

func runInspect(path string) {
	cfg, err := config.Load(path)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	reg, err := sites.Discover(cfg.Collect.NginxSitesDir, cfg.Collect.FpmPoolGlob)
	if err != nil {
		log.Fatalf("discover: %v", err)
	}
	fmt.Printf("%-34s %-6s %-38s %-52s %s\n", "USER", "UID", "DOMAIN", "ACCESS LOG", "POOLS")
	for _, s := range reg.Sites() {
		pools := make([]string, 0, len(s.Pools))
		for _, p := range s.Pools {
			pools = append(pools, p.Addr)
		}
		fmt.Printf("%-34s %-6d %-38s %-52s %s\n", s.User, s.UID, s.Domain, s.LogPath, strings.Join(pools, " "))
	}
}

// runWriteNginxMap prints the nginx map (domain -> FPM pool port) used by the
// /wstats-fpm-status location. Redirect its output to
// /etc/nginx/conf.d/website-stats-fpm.conf and reload nginx afterwards.
func runWriteNginxMap(path string) {
	cfg, err := config.Load(path)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	reg, err := sites.Discover(cfg.Collect.NginxSitesDir, cfg.Collect.FpmPoolGlob)
	if err != nil {
		log.Fatalf("discover: %v", err)
	}
	fmt.Println("# Generated by: website-stats write-nginx-map -- do not edit")
	fmt.Println("map $server_name $fpm_port {")
	fmt.Println("    default 0;")
	for _, s := range reg.Sites() {
		if len(s.Pools) == 0 {
			continue
		}
		addr := s.Pools[0].Addr
		idx := strings.LastIndex(addr, ":")
		if idx < 0 {
			continue
		}
		fmt.Printf("    %q %s;\n", s.Domain, addr[idx+1:])
	}
	fmt.Println("}")
}
