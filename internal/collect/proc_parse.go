package collect

import (
	"fmt"
	"strconv"
	"strings"
)

// Pure parsers for /proc content, kept free of I/O so they are unit-testable
// on any OS.

// parseStatTotal parses /proc/stat's first line ("cpu  u n s i iow irq sirq
// steal ...") and returns total and idle jiffies.
func parseStatTotal(content string) (total, idle uint64, err error) {
	for _, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		var sum, idleSum uint64
		for i, f := range fields {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return 0, 0, err
			}
			sum += v
			if i == 3 || i == 4 { // idle + iowait
				idleSum += v
			}
		}
		return sum, idleSum, nil
	}
	return 0, 0, fmt.Errorf("no cpu line in /proc/stat")
}

// parsePidStat parses /proc/<pid>/stat. Returns comm, utime+stime jiffies,
// rss in pages. The comm field may contain spaces, so everything after the
// final ')' is split.
func parsePidStat(content string) (comm string, jiffies uint64, rssPages uint64, err error) {
	open := strings.IndexByte(content, '(')
	close_ := strings.LastIndexByte(content, ')')
	if open < 0 || close_ < 0 || close_ < open {
		return "", 0, 0, fmt.Errorf("malformed stat line")
	}
	comm = content[open+1 : close_]
	rest := strings.Fields(content[close_+2:]) // field 3 (state) onwards
	if len(rest) < 22 {
		return "", 0, 0, fmt.Errorf("stat line too short")
	}
	utime, _ := strconv.ParseUint(rest[11], 10, 64) // field 14
	stime, _ := strconv.ParseUint(rest[12], 10, 64) // field 15
	rss, _ := strconv.ParseUint(rest[21], 10, 64)   // field 24
	return comm, utime + stime, rss, nil
}

// parseUid parses /proc/<pid>/status and returns the real UID.
func parseUid(content string) (int, error) {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "Uid:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				break
			}
			return strconv.Atoi(fields[1])
		}
	}
	return 0, fmt.Errorf("no Uid line")
}

// parseCmdline converts /proc/<pid>/cmdline (NUL-separated) to a display name.
func parseCmdline(content string) string {
	content = strings.TrimRight(content, "\x00")
	if content == "" {
		return ""
	}
	if i := strings.IndexByte(content, '\n'); i >= 0 {
		content = content[:i]
	}
	content = strings.ReplaceAll(content, "\x00", " ")
	return strings.TrimSpace(content)
}

// parseMeminfo returns MemTotal and MemAvailable in kB from /proc/meminfo.
func parseMeminfo(content string) (totalKB, availKB uint64) {
	for _, line := range strings.Split(content, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSpace(k) {
		case "MemTotal":
			totalKB = n
		case "MemAvailable":
			availKB = n
		}
	}
	return totalKB, availKB
}

// parseLoadavg returns the 1, 5 and 15-minute load averages.
func parseLoadavg(content string) (load1, load5, load15 float64) {
	fields := strings.Fields(content)
	if len(fields) < 3 {
		return
	}
	load1, _ = strconv.ParseFloat(fields[0], 64)
	load5, _ = strconv.ParseFloat(fields[1], 64)
	load15, _ = strconv.ParseFloat(fields[2], 64)
	return
}

// parseNetDev sums received/sent bytes over all interfaces except lo.
func parseNetDev(content string) (rx, tx uint64) {
	for _, line := range strings.Split(content, "\n") {
		iface, stats, ok := strings.Cut(line, ":")
		iface = strings.TrimSpace(iface)
		if !ok || iface == "" || iface == "lo" {
			continue
		}
		fields := strings.Fields(stats)
		if len(fields) < 9 {
			continue
		}
		r, _ := strconv.ParseUint(fields[0], 10, 64)
		t, _ := strconv.ParseUint(fields[8], 10, 64)
		rx += r
		tx += t
	}
	return rx, tx
}

// parseAccessLine parses one nginx access log line in the "main"/combined
// format:
//
//	IP - user [time] "request" status bytes "referer" "ua" "xff"
func parseAccessLine(line string) (ip, method, path string, status int, bytes int64, ua string, ok bool) {
	// client addr up to first space
	i := strings.IndexByte(line, ' ')
	if i <= 0 {
		return
	}
	ip = line[:i]
	rest := line[i+1:]

	// request inside the first pair of quotes
	q1 := strings.IndexByte(rest, '"')
	if q1 < 0 {
		return
	}
	q2 := strings.IndexByte(rest[q1+1:], '"')
	if q2 < 0 {
		return
	}
	request := rest[q1+1 : q1+1+q2]
	if m, p, found := strings.Cut(request, " "); found {
		method, path = m, p
		if j := strings.IndexByte(p, '?'); j >= 0 {
			path = p[:j]
		}
	} else {
		path = request
	}
	afterReq := rest[q1+1+q2+1:]

	fields := strings.Fields(afterReq)
	if len(fields) < 2 {
		return
	}
	status, err := strconv.Atoi(fields[0])
	if err != nil {
		return
	}
	if fields[1] == "-" {
		bytes = 0
	} else {
		b, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return
		}
		bytes = b
	}
	// quoted fields after status/bytes: "referer" "user-agent" ["xff"]
	ua = nthQuoted(afterReq, 2)
	return ip, method, path, status, bytes, ua, true
}

// nthQuoted returns the n-th (0-based) double-quoted segment of s, or "".
func nthQuoted(s string, n int) string {
	i := 0
	for {
		q1 := strings.IndexByte(s, '"')
		if q1 < 0 {
			return ""
		}
		rest := s[q1+1:]
		q2 := strings.IndexByte(rest, '"')
		if q2 < 0 {
			return ""
		}
		if i == n {
			return rest[:q2]
		}
		s = rest[q2+1:]
		i++
	}
}
