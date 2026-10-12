package sites

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseVhostFile(t *testing.T) {
	dir := t.TempDir()
	conf := `
server {
  listen 127.0.0.1:80;
  server_name example.com www.example.com;

  location ~ /.well-known/acme-challenge {
    access_log off;
  }

  access_log /home/exampleuser/logs/nginx/access.log main;

  location ~* \.(jpg|png)$ {
    access_log off;
  }
}
`
	path := filepath.Join(dir, "example.com.conf")
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	v := parseVhostFile(path)
	if v.domain != "example.com" {
		t.Fatalf("domain = %q", v.domain)
	}
	if v.logPath != "/home/exampleuser/logs/nginx/access.log" {
		t.Fatalf("logPath = %q", v.logPath)
	}
	if v.user != "exampleuser" {
		t.Fatalf("user = %q", v.user)
	}
}

func TestParsePool(t *testing.T) {
	dir := t.TempDir()
	conf := `
[example.com]
user = exampleuser
listen = 127.0.0.1:18003
pm = dynamic
pm.status_path = /status
`
	path := filepath.Join(dir, "example.com.conf")
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := parsePool(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.name != "example.com" || p.user != "exampleuser" || p.listen != "127.0.0.1:18003" || p.status != "/status" {
		t.Fatalf("pool = %+v", p)
	}
}
