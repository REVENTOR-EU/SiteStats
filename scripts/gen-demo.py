"""Builds build/repro/demo.html — the real dashboard UI rendered from an
anonymized snapshot of captured API data. Used to produce README screenshots.

Run from the repo root:  python scripts/gen-demo.py
Requires build/captured-overview.json (a captured /api/overview response).
"""
import io
import json
import os
import re
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
REPRO = os.path.join(ROOT, "build", "repro")
os.makedirs(REPRO, exist_ok=True)

# ---- 1. load + anonymize + scale the captured overview ----
ov = json.load(io.open(os.path.join(ROOT, "build", "captured-overview.json"), encoding="utf-8"))

fake_domains = ["shop-example.com", "blog-example.at", "agency-example.de", "portfolio-example.eu",
                "restaurant-example.at", "consulting-example.com", "news-example.eu", "wiki-example.org",
                "app-example.io", "community-example.net", "docs-example.dev", "status-example.live",
                "store-example.shop", "travel-example.at", "legal-example.de", "photo-example.com",
                "school-example.edu", "events-example.at", "clinic-example.com", "foundation-example.org"]
fake_names = ["shop", "blog", "agency", "portfolio", "restaurant", "consulting", "news", "wiki",
              "app", "community", "docs", "status", "store", "travel", "legal", "photo",
              "school", "events", "clinic", "foundation"]

ip_map = {}
def anon_ip(ip):
    if ip not in ip_map:
        ip_map[ip] = "198.51.100." + str(len(ip_map) % 254 + 1)
    return ip_map[ip]

SCALE = 6  # multiply CPU values so the demo chart bands are clearly visible

for i, s in enumerate(ov.get("sites", [])):
    if s["name"] == "_sys":
        s["domain"] = "System services"
    else:
        if i < len(fake_names):
            s["name"] = fake_names[i]
            s["domain"] = fake_domains[i]
    if s.get("series"):
        s["series"] = [[t, round(v * SCALE, 2) if isinstance(v, (int, float)) else v] for t, v in s["series"]]
    if s.get("topUrls"):
        for u in s["topUrls"]:
            u["value"] = u["value"].replace("translera", "myplugin")
    if s.get("topIps"):
        for u in s["topIps"]:
            u["value"] = anon_ip(u["value"])
ov["events"] = []

# ---- 2. copy the real UI assets ----
for name in ("uplot.iife.min.js", "app.js", "app.css"):
    src = io.open(os.path.join(ROOT, "internal", "server", "web", name), encoding="utf-8").read()
    io.open(os.path.join(REPRO, name), "w", encoding="utf-8", newline="\n").write(src)

# ---- 3. build demo.html from the real index.html ----
html = io.open(os.path.join(ROOT, "internal", "server", "web", "index.html"), encoding="utf-8").read()
html = html.replace("<title>website-stats · cp1</title>", "<title>website-stats demo</title>")
html = html.replace("cp1.reventor.eu", "demo.example.com")
html = html.replace("/static/", "")
html = re.sub(r"(\.js|\.css)\?v=\d+", r"\1", html)

overview_json = json.dumps(ov)
v = str(int(time.time()))
html = html.replace('<link rel="stylesheet" href="app.css">', f'<link rel="stylesheet" href="app.css?v={v}">')
html = html.replace('<script src="uplot.iife.min.js">', f'<script src="uplot.iife.min.js?v={v}">')
html = html.replace('<script src="app.js">', f'<script src="app.js?v={v}">')

stub = (
    "<script>\n"
    "const OVERVIEW = " + overview_json + ";\n"
    "window.fetch = (url) => {\n"
    "  const u = String(url);\n"
    '  let body = "[]";\n'
    '  if (u.includes("meta")) body = JSON.stringify({version:"1.0.0", sites:[]});\n'
    '  else if (u.includes("overview")) body = JSON.stringify(OVERVIEW);\n'
    '  return Promise.resolve(new Response(body, {status:200, headers:{"Content-Type":"application/json"}}));\n'
    "};\n"
    "</script>\n"
)
tag = '<script src="uplot.iife.min.js'
assert tag in html, "uplot script tag not found"
html = html.replace(tag, stub + tag)
assert "loginForm" in html

out = os.path.join(REPRO, "demo.html")
io.open(out, "w", encoding="utf-8", newline="\n").write(html)
print("demo.html written:", out)
