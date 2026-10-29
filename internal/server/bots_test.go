package server

import "testing"

func TestAsnOrgMatches(t *testing.T) {
	if !asnOrgMatches("meta-webindexer", "Facebook, Inc.") {
		t.Error("meta-webindexer should match Facebook, Inc.")
	}
	if !asnOrgMatches("meta-externalagent", "META-PUBLIC") {
		t.Error("meta tokens should match META-PUBLIC")
	}
	if !asnOrgMatches("Googlebot", "GOOGLE-GOOGLE") {
		t.Error("googlebot should match google ASN org")
	}
	if asnOrgMatches("Baiduspider", "CHINA UNICOM China16") {
		t.Error("china unicom must NOT verify a baidu claim")
	}
	if asnOrgMatches("Uptime-Kuma", "whatever") {
		t.Error("self-hosted monitors cannot be network-verified")
	}
}
