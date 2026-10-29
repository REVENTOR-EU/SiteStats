// Package geo enriches IPs with country/city and network-owner (ASN) data
// from local MMDB files (DBIP Lite or MaxMind GeoLite2). Both files are
// optional: when absent the lookups simply return empty results, so the
// dashboard works without them.
package geo

import (
	"net"
	"strings"
	"sync"
	"time"

	"github.com/oschwald/geoip2-golang"
)

type Lookup struct {
	Country string // ISO-2 country code, "" if unknown
	City    string // English city name, "" if unknown
	ASN     uint   // autonomous system number, 0 if unknown
	ASNOrg  string // organization name, "" if unknown
}

type DB struct {
	mu     sync.Mutex
	city   *geoip2.Reader
	asn    *geoip2.Reader
	loaded time.Time
	cityFn string
	asnFn  string
}

// Open opens the country and ASN databases; missing files are tolerated.
// Call Reload periodically to pick up refreshed downloads.
func Open(cityFile, asnFile string) *DB {
	return &DB{cityFn: cityFile, asnFn: asnFile}
}

// Reload (re)opens the mmdb files if they exist and changed on disk.
func (d *DB) Reload() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, err := geoip2.Open(d.cityFn); err == nil {
		if d.city != nil {
			d.city.Close()
		}
		d.city = c
	}
	if a, err := geoip2.Open(d.asnFn); err == nil {
		if d.asn != nil {
			d.asn.Close()
		}
		d.asn = a
	}
	d.loaded = time.Now()
}

// Loaded reports whether at least one database is readable.
func (d *DB) Loaded() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.city != nil || d.asn != nil
}

func (d *DB) Lookup(ip net.IP) Lookup {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out Lookup
	if ip == nil {
		return out
	}
	if d.city != nil {
		if rec, err := d.city.Country(ip); err == nil {
			out.Country = rec.Country.IsoCode
		}
		if rec, err := d.city.City(ip); err == nil {
			out.City = rec.City.Names["en"]
		}
	}
	if d.asn != nil {
		if rec, err := d.asn.ASN(ip); err == nil {
			out.ASN = rec.AutonomousSystemNumber
			out.ASNOrg = strings.TrimSpace(rec.AutonomousSystemOrganization)
		}
	}
	return out
}
