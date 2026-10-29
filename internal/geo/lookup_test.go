package geo

import (
	"net"
	"testing"
)

func TestLookupRealDB(t *testing.T) {
	db := Open("../../build/geo/country.mmdb", "../../build/geo/asn.mmdb")
	db.Reload()
	if !db.Loaded() {
		t.Skip("mmdb files not present")
	}
	g := db.Lookup(net.ParseIP("2a03:2880:f806:39::")) // Meta crawler range
	t.Logf("meta: %+v", g)
	if g.ASNOrg == "" {
		t.Fatalf("expected ASN org for Meta range")
	}
	gl := db.Lookup(net.ParseIP("216.73.216.226")) // Anthropic crawler on AWS
	t.Logf("anthropic: %+v", gl)
	gl2 := db.Lookup(net.ParseIP("185.191.171.9")) // Semrush
	t.Logf("semrush: %+v", gl2)
}
