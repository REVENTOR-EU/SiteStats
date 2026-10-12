"""Regenerates build/repro/demo.html from the real index.html with a stubbed
fetch serving build/repro/overview.json. Used to produce demo screenshots."""
import io
import json
import os
import re

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

html = io.open(os.path.join(ROOT, "internal", "server", "web", "index.html"), encoding="utf-8").read()
html = html.replace("<title>website-stats · cp1</title>", "<title>website-stats demo</title>")
html = html.replace("cp1.reventor.eu", "demo.example.com")
html = html.replace("/static/", "")
html = re.sub(r"(\.js|\.css)\?v=\d+", r"\1", html)
overview = io.open(os.path.join(ROOT, "build", "repro", "overview.json"), encoding="utf-8").read()

stub = (
    "<script>\n"
    "const OVERVIEW = " + overview + ";\n"
    "window.fetch = (url) => {\n"
    "  const u = String(url);\n"
    '  let body = "[]";\n'
    '  if (u.includes("meta")) body = JSON.stringify({version:"1.0.0", sites:[]});\n'
    '  else if (u.includes("overview")) body = JSON.stringify(OVERVIEW);\n'
    '  return Promise.resolve(new Response(body, {status:200, headers:{"Content-Type":"application/json"}}));\n'
    "};\n"
    "</script>\n"
    '<script src="uplot.iife.min.js"></script>'
)
tag = '<script src="uplot.iife.min.js"></script>'
assert tag in html, "uplot script tag not found in index.html"
html = html.replace(tag, stub)
assert "loginForm" in html

out = os.path.join(ROOT, "build", "repro", "demo.html")
io.open(out, "w", encoding="utf-8", newline="\n").write(html)
print("demo.html written:", out)
