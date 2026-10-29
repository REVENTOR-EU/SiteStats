package server

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"
)

// botVendor is one known crawler family: the user-agent token it advertises
// and the reverse-DNS domains its real crawlers live under.
type botVendor struct {
	token   string   // UA substring that claims this identity
	domains []string // rDNS suffixes owned by the vendor
}

var botVendors = []botVendor{
	{"Googlebot", []string{"googlebot.com", "google.com"}},
	{"Google-InspectionTool", []string{"googlebot.com", "google.com"}},
	{"Bingbot", []string{"search.msn.com", "bing.com"}},
	{"DuckDuckBot", []string{"duckduckgo.com"}},
	{"Baiduspider", []string{"baidu.com", "baiduspider.com"}},
	{"YandexBot", []string{"yandex.com", "yandex.net", "yandex.ru"}},
	{"Applebot", []string{"applebot.apple.com", "apple.com"}},
	{"SemrushBot", []string{"semrush.com", "semrushbot.com"}},
	{"AhrefsBot", []string{"ahrefs.com"}},
	{"MJ12bot", []string{"mj12bot.com", "majestic12.co.uk"}},
	{"PetalBot", []string{"petalsearch.com", "petalbot.com"}},
	{"GPTBot", []string{"openai.com"}},
	{"OAI-SearchBot", []string{"openai.com"}},
	{"ChatGPT-User", []string{"openai.com"}},
	{"ClaudeBot", []string{"anthropic.com", "claudebot.com"}},
	{"Claude-User", []string{"anthropic.com"}},
	{"Claude-SearchBot", []string{"anthropic.com"}},
	{"anthropic-ai", []string{"anthropic.com"}},
	{"PerplexityBot", []string{"perplexity.ai", "perplexitybot.com"}},
	{"Amazonbot", []string{"amazon.com"}},
	{"FacebookBot", []string{"facebook.com"}},
	{"meta-externalagent", []string{"facebook.com", "meta.com"}},
	{"meta-webindexer", []string{"facebook.com", "meta.com"}},
	{"Twitterbot", []string{"twitter.com", "x.com"}},
	{"LinkedInBot", []string{"linkedin.com"}},
	{"Slurp", []string{"yahoo.com", "inktomisearch.com"}},
	{"Bytespider", []string{"bytedance.com", "bytespider.com"}},
	{"DotBot", []string{"dotmoz.com"}},
	{"Screaming Frog", []string{"screamingfrog.co.uk"}},
}

// BotVerdict is the classification of one (IP, user-agent) pair.
type BotVerdict struct {
	Vendor   string `json:"vendor"`   // advertised vendor token, "" if none
	Verified string `json:"verified"` // "yes" | "fake" (PTR exists, wrong domain) | "noptr" (no PTR published) | "" (not a bot token)
}

// botCache remembers rDNS verdicts per IP so the live log doesn't hammer the
// resolver. Entries expire so legitimately-moving IPs get re-checked.
type botCache struct {
	mu      sync.Mutex
	m       map[string]botEntry
	res     *net.Resolver
	timeout time.Duration
	ttl     time.Duration
}

type botEntry struct {
	v    BotVerdict
	done time.Time
}

func newBotCache() *botCache {
	return &botCache{
		m:       map[string]botEntry{},
		res:     net.DefaultResolver,
		timeout: 3 * time.Second,
		ttl:     6 * time.Hour,
	}
}

// Classify returns the verdict for (ip, ua). A UA without any known bot
// token counts as an ordinary client (no verification needed). A UA claiming
// a known bot is verified by forward-confirmed reverse DNS: the IP must
// resolve to a hostname under the vendor's domains that resolves back to the
// same IP. Claimed-but-unverified => "no" (faked identifier).
func (b *botCache) Classify(ip, ua string) BotVerdict {
	vendor := ""
	for _, v := range botVendors {
		if strings.Contains(ua, v.token) {
			vendor = v.token
			break
		}
	}
	if vendor == "" {
		return BotVerdict{}
	}

	b.mu.Lock()
	if e, ok := b.m[ip]; ok && time.Now().Before(e.done) {
		b.mu.Unlock()
		return e.v
	}
	b.mu.Unlock()

	verdict := BotVerdict{Vendor: vendor, Verified: "noptr"}
	ok, hasPTR := b.forwardConfirm(ip, vendor)
	switch {
	case ok:
		verdict.Verified = "yes"
	case hasPTR:
		verdict.Verified = "fake" // reverse DNS exists but points elsewhere
	}

	b.mu.Lock()
	b.m[ip] = botEntry{v: verdict, done: time.Now().Add(b.ttl)}
	b.mu.Unlock()
	return verdict
}

// forwardConfirm reports (forward-confirmed, any PTR existed at all).
func (b *botCache) forwardConfirm(ip, vendor string) (bool, bool) {
	var domains []string
	for _, v := range botVendors {
		if v.token == vendor {
			domains = v.domains
			break
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	names, err := b.res.LookupAddr(ctx, ip)
	if err != nil || len(names) == 0 {
		return false, false // no PTR published: identity cannot be checked
	}
	for _, name := range names {
		name = strings.TrimSuffix(name, ".")
		for _, d := range domains {
			if name == d || strings.HasSuffix(name, "."+d) {
				// forward check: hostname must resolve back to the IP
				ips, err := b.res.LookupHost(ctx, name)
				if err != nil {
					continue
				}
				for _, a := range ips {
					if a == ip {
						return true, true
					}
				}
			}
		}
	}
	return false, true
}
