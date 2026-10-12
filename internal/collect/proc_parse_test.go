package collect

import (
	"testing"
)

func TestParseStatTotal(t *testing.T) {
	content := "cpu  100 0 200 700 50 0 10 40 0 0\ncpu0 50 0 100 350 25 0 5 20 0 0\nintr 123\n"
	total, idle, err := parseStatTotal(content)
	if err != nil {
		t.Fatal(err)
	}
	// 100+0+200+700+50+0+10+40+0+0 = 1100; idle = 700 + 50 = 750
	if total != 1100 {
		t.Fatalf("total = %d, want 1100", total)
	}
	if idle != 750 {
		t.Fatalf("idle = %d, want 750", idle)
	}
}

func TestParsePidStat(t *testing.T) {
	line := "1234 (php-fpm: pool example-site) S 1 1234 1234 0 -1 4194560 100 0 0 0 250 150 0 0 20 0 5 0 5678 948388768 17800 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 17 4 0 0 0 0 0"
	comm, jiffies, rss, err := parsePidStat(line)
	if err != nil {
		t.Fatal(err)
	}
	if comm != "php-fpm: pool example-site" {
		t.Fatalf("comm = %q", comm)
	}
	if jiffies != 400 {
		t.Fatalf("jiffies = %d, want 400 (250 utime + 150 stime)", jiffies)
	}
	if rss != 17800 {
		t.Fatalf("rss = %d, want 17800", rss)
	}
}

func TestParsePidStatSpaceyComm(t *testing.T) {
	line := "99 (chromium --type=rend) R 1 1 1 0 -1 0 0 0 0 0 5 6 0 0 0 0 1 0 1 0 2 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0"
	comm, jiffies, _, err := parsePidStat(line)
	if err != nil {
		t.Fatal(err)
	}
	if comm != "chromium --type=rend" {
		t.Fatalf("comm = %q", comm)
	}
	if jiffies != 11 {
		t.Fatalf("jiffies = %d, want 11", jiffies)
	}
}

func TestParseUid(t *testing.T) {
	content := "Name:\tphp-fpm\nUid:\t1013\t1013\t1013\t1013\nGid:\t1013\t1013\t1013\t1013\n"
	uid, err := parseUid(content)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 1013 {
		t.Fatalf("uid = %d, want 1013", uid)
	}
}

func TestParseCmdline(t *testing.T) {
	if got := parseCmdline("php-fpm: pool hereandnow.co.at\x00"); got != "php-fpm: pool hereandnow.co.at" {
		t.Fatalf("got %q", got)
	}
	if got := parseCmdline("\x00"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestParseMeminfo(t *testing.T) {
	content := "MemTotal:       8032964 kB\nMemFree:         123456 kB\nMemAvailable:   4900000 kB\nSwapTotal:             0 kB\n"
	total, avail := parseMeminfo(content)
	if total != 8032964 {
		t.Fatalf("total = %d", total)
	}
	if avail != 4900000 {
		t.Fatalf("avail = %d", avail)
	}
}

func TestParseNetDev(t *testing.T) {
	content := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 1000000    5000    0    0    0     0          0         0  1000000    5000    0    0    0     0       0          0
  eth0: 9000000   10000    0    0    0     0          0         0 4000000    8000    0    0    0     0       0          0
`
	rx, tx := parseNetDev(content)
	if rx != 9000000 {
		t.Fatalf("rx = %d, want 9000000 (lo excluded)", rx)
	}
	if tx != 4000000 {
		t.Fatalf("tx = %d, want 4000000", tx)
	}
}

func TestParseAccessLine(t *testing.T) {
	line := `203.0.113.7 - - [12/Oct/2026:20:25:41 +0200] "POST /wp-cron.php?doing_wp_cron=1 HTTP/1.1" 200 11 "-" "WordPress/7.1.3; https://example-site.com" "-"`
	ip, method, path, status, bytes, ok := parseAccessLine(line)
	if !ok {
		t.Fatal("expected ok")
	}
	if ip != "203.0.113.7" || method != "POST" || path != "/wp-cron.php" || status != 200 || bytes != 11 {
		t.Fatalf("got %q %q %q %d %d", ip, method, path, status, bytes)
	}
}

func TestParseAccessLineZeroBytesAndSpacedPath(t *testing.T) {
	line := `130.61.26.101 - - [12/Oct/2026:20:25:41 +0200] "GET /some/path with space?q=1 HTTP/1.1" 404 - "-" "Uptime-Kuma/2.2.0" "-"`
	ip, _, path, status, bytes, ok := parseAccessLine(line)
	if !ok {
		t.Fatal("expected ok")
	}
	if ip != "130.61.26.101" || status != 404 || bytes != 0 {
		t.Fatalf("got %q %d %d", ip, status, bytes)
	}
	if path != "/some/path with space" {
		t.Fatalf("path = %q", path)
	}
}

func TestParseAccessLineGarbage(t *testing.T) {
	for _, line := range []string{"", "not a log line", `1.2.3.4 - - [x] "GET / HTTP/1.1" abc def "-" "-" "-"`} {
		if _, _, _, _, _, ok := parseAccessLine(line); ok {
			t.Fatalf("expected !ok for %q", line)
		}
	}
}

func TestParseAccessLineTruncated(t *testing.T) {
	line := `1.2.3.4 - - [12/Oct/2026:20:25:41 +0200] "GET / HTTP/1.1"`
	if _, _, _, _, _, ok := parseAccessLine(line); ok {
		t.Fatal("expected !ok for truncated line")
	}
}

func TestEncodeNV(t *testing.T) {
	got := encodeNV("abc")
	if got[0] != 3 || string(got[1:]) != "abc" {
		t.Fatalf("short nv = %v", got)
	}
	long := make([]byte, 300)
	for i := range long {
		long[i] = 'x'
	}
	gotLong := encodeNV(string(long))
	if len(gotLong) != 304 || gotLong[0]&0x80 == 0 {
		t.Fatalf("long nv encoding wrong: len=%d first=%x", len(gotLong), gotLong[0])
	}
}
