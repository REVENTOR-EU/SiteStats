package server

import (
	"net/http/httptest"
	"testing"
)

func TestParseRange(t *testing.T) {
	cases := map[string]int64{
		"15m": 900, "1h": 3600, "6h": 6 * 3600, "24h": 86400,
		"7d": 7 * 86400, "30d": 30 * 86400, "": 86400, "bogus": 86400,
	}
	for in, want := range cases {
		r := httptest.NewRequest("GET", "/api/overview?range="+in, nil)
		if got := parseRange(r); got != want {
			t.Errorf("parseRange(%q) = %d (%.1f days), want %d", in, got, float64(got)/86400, want)
		}
	}
}
