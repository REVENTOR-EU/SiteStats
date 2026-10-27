package collect

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// snippet returns the first 80 bytes of s for error messages.
func snippet(s []byte) string {
	if len(s) > 80 {
		s = s[:80]
	}
	return string(s)
}

// Minimal FastCGI ( responder role ) client, enough to fetch a PHP-FPM pool
// status page over TCP.

const (
	fcgiBeginRequest = 1
	fcgiEndRequest   = 3
	fcgiParams       = 4
	fcgiStdin        = 5
	fcgiStdout       = 6
	fcgiStderr       = 7
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

	// PARAMS — name-value pairs are encoded as nameLen, valueLen, name,
	// value (both lengths together, BEFORE the data)
	params := map[string]string{
		"SCRIPT_NAME":     statusPath,
		"SCRIPT_FILENAME": statusPath,
		"QUERY_STRING":    "json",
		"REQUEST_METHOD":  "GET",
		"SERVER_PROTOCOL": "HTTP/1.1",
	}
	var nv []byte
	for k, v := range params {
		nv = append(nv, encodeNVPair(k, v)...)
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
		case fcgiStderr:
			// ignore pool stderr chatter
		case fcgiEndRequest:
			// stdout is CGI-style: optional "Status: ..." and Content-type
			// headers, blank line, then the body — skip to the JSON
			if i := bytes.IndexByte(out, '{'); i > 0 {
				out = out[i:]
			}
			if len(out) == 0 || out[0] != '{' {
				return nil, fmt.Errorf("no JSON status from %s (pm.status_path set?): %q", addr, snippet(out))
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

// encodeNVPair encodes one FastCGI name-value pair: the two lengths come
// first (each 1 byte, or 4 bytes big-endian with the high bit set for
// lengths >= 128), followed by the name bytes and the value bytes.
func encodeNVPair(k, v string) []byte {
	lenBytes := func(n int) []byte {
		if n < 128 {
			return []byte{byte(n)}
		}
		b := make([]byte, 4)
		b[0] = byte(n>>24) | 0x80
		b[1] = byte(n >> 16)
		b[2] = byte(n >> 8)
		b[3] = byte(n)
		return b
	}
	out := lenBytes(len(k))
	out = append(out, lenBytes(len(v))...)
	out = append(out, k...)
	return append(out, v...)
}

// FPM pool status is queried directly over FastCGI from the pool's listen
// address (discovered from its pool.d config, including its pm.status_path);
// no nginx location is needed.

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
