#!/usr/bin/env python3
"""Tail log files, redact them, and serve a live web page + JSON API. Stdlib only.

  python3 logstream.py --logs-dir /workspace/logs --port 8080 --status-file /workspace/run/status.json

Endpoints (read-only, no-store):
  /                      the page (index.html next to this file)
  /api/lines?after=SEQ   lines with seq > SEQ (max --page-size), plus last_seq
  /api/status            contents of --status-file (also passed through the redactor)
  /healthz               ok

Lines never reach the ring buffer unredacted. Bind to 127.0.0.1 and publish through a tunnel.
"""
import argparse
import collections
import glob
import json
import os
import re
import sys
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from redact import redact, should_drop  # noqa: E402

TS_RE = re.compile(r"^(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2})")
ANSI_RE = re.compile(r"\x1b\[[0-9;?]*[A-Za-z]")
LEVEL_RE = re.compile(r"\b(ERROR|CRITICAL|WARN(?:ING)?|INFO|DEBUG)\b")


class Buffer:
    def __init__(self, max_lines):
        self.lines = collections.deque(maxlen=max_lines)
        self.seq = 0
        self.lock = threading.Lock()

    def add(self, src, text):
        text = ANSI_RE.sub("", text).rstrip("\r\n")
        if not text.strip() or should_drop(text):
            return
        text = redact(text)[:2000]
        m = TS_RE.match(text)
        lvl = LEVEL_RE.search(text)
        kind = "info"
        if lvl:
            kind = {"ERROR": "error", "CRITICAL": "error", "WARN": "warn", "WARNING": "warn",
                    "DEBUG": "debug"}.get(lvl.group(1), "info")
        if "Block found!" in text or "Block accepted by node!" in text or "verified ZK proof" in text or "block found" in text.lower():
            kind = "block"
        elif text.lstrip().startswith("[") and "] accepted (" in text:
            kind = "info"
        with self.lock:
            self.seq += 1
            self.lines.append({"seq": self.seq, "t": int(time.time()), "ts": m.group(1) if m else None,
                               "src": src, "kind": kind, "text": text})

    def after(self, seq, limit):
        with self.lock:
            out = [l for l in self.lines if l["seq"] > seq]
            return out[-limit:], self.seq


def tail_file(path, src, buf, backfill):
    """Follow a file across truncation/rotation. Start with the last `backfill` lines."""
    pos = 0
    f = None
    first = True
    while True:
        try:
            if f is None:
                f = open(path, "rb")
                if first:
                    f.seek(0, 2)
                    size = f.tell()
                    f.seek(max(0, size - 256 * 1024))
                    for line in f.read().decode("utf-8", "replace").splitlines()[-backfill:]:
                        buf.add(src, line)
                    first = False
                pos = f.tell()
            chunk = f.read()
            if chunk:
                pos += len(chunk)
                for line in chunk.decode("utf-8", "replace").splitlines():
                    buf.add(src, line)
            else:
                try:
                    st = os.stat(path)
                    if st.st_size < pos or st.st_ino != os.fstat(f.fileno()).st_ino:
                        f.close()
                        f = None  # rotated or truncated: reopen
                except FileNotFoundError:
                    f.close()
                    f = None
                time.sleep(0.5)
        except FileNotFoundError:
            time.sleep(2)
        except Exception as e:  # keep tailing no matter what
            buf.add("logstream", f"tail error on {src}: {e}")
            time.sleep(2)


def watch_dir(logs_dir, buf, backfill, started):
    """Pick up *.log files that appear later (e.g. vllm.log after sync finishes)."""
    while True:
        for path in sorted(glob.glob(os.path.join(logs_dir, "*.log"))):
            if path in started:
                continue
            src = os.path.splitext(os.path.basename(path))[0]
            started.add(path)
            threading.Thread(target=tail_file, args=(path, src, buf, backfill), daemon=True).start()
        time.sleep(3)


def push_loop(buf, status_file, url, token, interval):
    """Ship new lines + the latest status to the site every `interval` seconds.

    Lines come out of the same redacted buffer the local page serves, so the site never sees
    anything the local page would not. Retries with backoff; the cursor only advances on a 2xx.
    """
    pushed = 0
    backoff = interval
    announced = False
    while True:
        time.sleep(backoff)
        lines, last = buf.after(pushed, 500)
        status = None
        if status_file:
            try:
                with open(status_file) as f:
                    status = json.loads(redact(f.read()))
            except Exception:
                status = None
        body = json.dumps({"lines": [{k: l[k] for k in ("t", "ts", "src", "kind", "text")} for l in lines],
                           "status": status}).encode()
        req = urllib.request.Request(url, data=body, method="POST",
                                     headers={"Content-Type": "application/json", "X-Miner-Token": token,
                                              "User-Agent": "pearl-logstream/1"})
        try:
            with urllib.request.urlopen(req, timeout=15) as r:
                r.read()
            if lines:
                pushed = lines[-1]["seq"]
            if not announced:
                buf.add("logstream", f"log push to site is live ({len(lines)} lines backfilled)")
                announced = True
            backoff = interval
        except urllib.error.HTTPError as e:
            if e.code in (401, 404):
                buf.add("logstream", f"site rejected push ({e.code}): check LOGS_PUSH_URL / LOGS_PUSH_TOKEN; retrying in 60s")
                backoff = 60
            else:
                backoff = min(30, backoff * 2)
        except Exception:
            backoff = min(30, backoff * 2)


def make_handler(buf, status_file, title, page_size, index_html):
    class H(BaseHTTPRequestHandler):
        server_version = "pearl-logstream/1"

        def log_message(self, *a):  # keep our own stdout quiet
            pass

        def _send(self, code, body, ctype):
            self.send_response(code)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "no-store")
            self.send_header("X-Content-Type-Options", "nosniff")
            self.send_header("Referrer-Policy", "no-referrer")
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self):
            u = urlparse(self.path)
            if u.path == "/":
                html = index_html.replace("__TITLE__", title)
                return self._send(200, html.encode(), "text/html; charset=utf-8")
            if u.path == "/healthz":
                return self._send(200, b"ok", "text/plain")
            if u.path == "/api/lines":
                q = parse_qs(u.query)
                try:
                    after = int(q.get("after", ["0"])[0])
                except ValueError:
                    after = 0
                lines, last = buf.after(after, page_size)
                body = json.dumps({"lines": lines, "last_seq": last, "server_time": int(time.time())})
                return self._send(200, body.encode(), "application/json")
            if u.path == "/api/status":
                try:
                    with open(status_file) as f:
                        raw = f.read()
                    data = json.loads(redact(raw))
                except Exception:
                    data = {"error": "status unavailable"}
                data["served_at"] = int(time.time())
                return self._send(200, json.dumps(data).encode(), "application/json")
            self._send(404, b"not found", "text/plain")

    return H


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--logs-dir", required=True)
    ap.add_argument("--status-file", default="")
    ap.add_argument("--port", type=int, default=8080)
    ap.add_argument("--bind", default="127.0.0.1")
    ap.add_argument("--title", default="Pearl solo miner")
    ap.add_argument("--max-lines", type=int, default=5000)
    ap.add_argument("--backfill", type=int, default=200)
    ap.add_argument("--page-size", type=int, default=500)
    ap.add_argument("--push-url", default="", help="site ingest URL, e.g. https://site/api/miners/<id>/ingest")
    ap.add_argument("--push-token", default="", help="per-rig token issued by the site")
    ap.add_argument("--push-interval", type=float, default=3.0)
    a = ap.parse_args()

    with open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "index.html")) as f:
        index_html = f.read()

    buf = Buffer(a.max_lines)
    started = set()
    threading.Thread(target=watch_dir, args=(a.logs_dir, buf, a.backfill, started), daemon=True).start()
    buf.add("logstream", f"log streamer started, serving {a.logs_dir} on {a.bind}:{a.port}")
    if a.push_url and a.push_token:
        threading.Thread(target=push_loop, args=(buf, a.status_file, a.push_url, a.push_token, a.push_interval), daemon=True).start()
        buf.add("logstream", f"pushing logs to {a.push_url.split('/api/')[0]} every {a.push_interval:g}s")

    srv = ThreadingHTTPServer((a.bind, a.port), make_handler(buf, a.status_file, a.title, a.page_size, index_html))
    srv.daemon_threads = True
    print(f"logstream: http://{a.bind}:{a.port}/", flush=True)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
