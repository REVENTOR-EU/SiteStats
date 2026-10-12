package collect

import (
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Minimal FastCGI ( responder role ) client, enough to fetch a PHP-FPM pool
// status page over TCP.

const (
	fcgiBeginRequest = 1
	fcgiEndRequest   = 3
	fcgiParams       = 4
	fcgiStdin        = 5
	fcgiStdout       = 6
)

type fpmJSON struct {
	Pool               string `json:"pool"`
	AcceptedConn       int64  `json:"accepted conn"`
	ListenQueue        int    `json:"listen queue"`
	MaxListenQueue     int    `json:"max listen queue"`
	IdleProcesses      int    `json:"idle processes"`
	ActiveProcesses    int    `json:"active processes"`
	TotalProcesses     int    `json:"total processes"`
	MaxActiveProcesses int    `json:"max active processes"`
	MaxChildrenReached int64  `json:"max children reached"`
	SlowRequests       int64  `json:"slow requests"`
}

// QueryFpmStatus fetches the pool status page (as JSON) from addr.
func QueryFpmStatus(addr, statusPath string, timeout time.Duration) (*FpmStatus, error) {
	if statusPath == "" {
		statusPath = "/status"
	}
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}

	var reqID uint16 = 1
	writeRecord := func(typ byte, body []byte) error {
		hdr := make([]byte, 8)
		hdr[0] = 1
		hdr[1] = typ
		binary.BigEndian.PutUint16(hdr[2:], reqID)
		binary.BigEndian.PutUint16(hdr[4:], uint16(len(body)))
		if _, err := conn.Write(append(hdr, body...)); err != nil {
			return err
		}
		return nil
	}

	// BEGIN_REQUEST: role=responder(1), flags=0
	br := make([]byte, 8)
	binary.BigEndian.PutUint16(br, 1)
	if err := writeRecord(fcgiBeginRequest, br); err != nil {
		return nil, err
	}

	// PARAMS
	params := map[string]string{
		"SCRIPT_NAME":     statusPath,
		"QUERY_STRING":    "json",
		"REQUEST_METHOD":  "GET",
		"SERVER_PROTOCOL": "HTTP/1.1",
	}
	var nv []byte
	for k, v := range params {
		nv = append(nv, encodeNV(k)...)
		nv = append(nv, encodeNV(v)...)
	}
	if err := writeRecord(fcgiParams, nv); err != nil {
		return nil, err
	}
	if err := writeRecord(fcgiParams, nil); err != nil { // end of params
		return nil, err
	}
	if err := writeRecord(fcgiStdin, nil); err != nil { // end of input
		return nil, err
	}

	// read response records
	var out []byte
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(conn, hdr); err != nil {
			return nil, err
		}
		length := binary.BigEndian.Uint16(hdr[4:])
		pad := hdr[6]
		body := make([]byte, int(length)+int(pad))
		if _, err := io.ReadFull(conn, body); err != nil {
			return nil, err
		}
		switch hdr[1] {
		case fcgiStdout:
			out = append(out, body[:length]...)
		case fcgiEndRequest:
			if len(out) == 0 {
				return nil, fmt.Errorf("empty status response from %s (pm.status_path set?)", addr)
			}
			var j fpmJSON
			if err := json.Unmarshal(out, &j); err != nil {
				return nil, fmt.Errorf("status from %s is not JSON (pm.status_path set?): %w", addr, err)
			}
			return &FpmStatus{
				Pool:           j.Pool,
				Active:         j.ActiveProcesses,
				Idle:           j.IdleProcesses,
				TotalProcs:     j.TotalProcesses,
				ListenQueue:    j.ListenQueue,
				MaxListenQueue: j.MaxListenQueue,
				AcceptedConn:   j.AcceptedConn,
				MaxChildren:    j.MaxChildrenReached,
				SlowRequests:   j.SlowRequests,
			}, nil
		}
	}
}

func encodeNV(s string) []byte {
	l := len(s)
	if l < 128 {
		return append([]byte{byte(l)}, s...)
	}
	b := make([]byte, 4, 4+len(s))
	b[0] = byte(l>>24) | 0x80
	b[1] = byte(l >> 16)
	b[2] = byte(l >> 8)
	b[3] = byte(l)
	return append(b, s...)
}

// FPM pool status is fetched through the local nginx: each CloudPanel vhost
// exposes an internal, loopback-only location (/wstats-fpm-status by default)
// that fastcgi_passes to the site's pool. Direct FastCGI dialing is refused
// by this server's PHP-FPM for non-nginx clients, so nginx is the reliable
// route.

// QueryFpmStatusViaNginx fetches the pool status page (JSON) for a site from
// the local nginx vhost. TLS SNI and Host select the vhost; the certificate
// is not verified (we connect to 127.0.0.1, nginx is a trusted local service).
func QueryFpmStatusViaNginx(domain, statusPath string, timeout time.Duration) (*FpmStatus, error) {
	if statusPath == "" {
		statusPath = "/wstats-fpm-status"
	}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{ServerName: domain, InsecureSkipVerify: true},
		DialContext:     (&net.Dialer{Timeout: timeout}).DialContext,
	}
	client := &http.Client{Timeout: timeout, Transport: transport}
	req, err := http.NewRequest(http.MethodGet, "https://127.0.0.1"+statusPath, nil)
	if err != nil {
		return nil, err
	}
	req.Host = domain
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status endpoint for %s returned %s (nginx map missing?)", domain, resp.Status)
	}
	var j fpmJSON
	if err := json.NewDecoder(resp.Body).Decode(&j); err != nil {
		return nil, fmt.Errorf("decode status for %s: %w", domain, err)
	}
	return &FpmStatus{
		Pool:           j.Pool,
		Active:         j.ActiveProcesses,
		Idle:           j.IdleProcesses,
		TotalProcs:     j.TotalProcesses,
		ListenQueue:    j.ListenQueue,
		MaxListenQueue: j.MaxListenQueue,
		AcceptedConn:   j.AcceptedConn,
		MaxChildren:    j.MaxChildrenReached,
		SlowRequests:   j.SlowRequests,
	}, nil
}
