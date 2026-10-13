#!/usr/bin/env python3
"""A tiny registry-v2 manifest mirror for the live acceptance run.

It answers /v2/ and HEAD/GET manifests with a valid OCI manifest, and appends
every request to a log file so the caller can see which mirror was asked.

usage: fake-mirror.py --port N --delay-ms N --marker NAME --log PATH [--repo R]

--repo restricts the mirror to the repositories it pretends to hold: a request
for anything else is answered 404, which is what keeps one fixture namespace
from silently serving another namespace's repository (a mirror that answers
every path makes every namespace "have" everything, and the namespace race then
picks by latency rather than by content).
"""
import argparse
import hashlib
import json
import os
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

args = None

# A valid OCI manifest: one config descriptor and one layer descriptor. The
# blobs themselves do not exist — the acceptance run only fetches manifests.
CONFIG_DIGEST = "sha256:" + "c" * 64
LAYER_DIGEST = "sha256:" + "1" * 64


def manifest_body(marker: str) -> bytes:
    doc = {
        "schemaVersion": 2,
        "mediaType": "application/vnd.oci.image.manifest.v1+json",
        "annotations": {"org.rms.acceptance.marker": marker},
        "config": {
            "mediaType": "application/vnd.oci.image.config.v1+json",
            "digest": CONFIG_DIGEST,
            "size": 2,
        },
        "layers": [
            {
                "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
                "digest": LAYER_DIGEST,
                "size": 2,
            }
        ],
    }
    return json.dumps(doc, sort_keys=True).encode()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):  # keep stdout clean; the log file is the record
        pass

    def record(self, method):
        with open(args.log, "a") as fh:
            fh.write("%s %s\n" % (method, self.path))

    def answer(self, method):
        self.record(method)
        if args.delay_ms:
            time.sleep(args.delay_ms / 1000.0)

        if self.path == "/v2/" or self.path == "/v2":
            body = b"{}"
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            if method == "GET":
                self.wfile.write(body)
            return

        if "/manifests/" in self.path:
            repo = self.path.split("/v2/", 1)[-1].split("/manifests/", 1)[0]
            if args.repo and repo not in args.repo.split(","):
                self.send_response(404)
                self.send_header("Content-Length", "0")
                self.end_headers()
                return
            body = manifest_body(args.marker)
            digest = "sha256:" + hashlib.sha256(body).hexdigest()
            self.send_response(200)
            self.send_header("Content-Type", "application/vnd.oci.image.manifest.v1+json")
            self.send_header("Docker-Content-Digest", digest)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            if method == "GET":
                self.wfile.write(body)
            return

        self.send_response(404)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_HEAD(self):
        self.answer("HEAD")

    def do_GET(self):
        self.answer("GET")


def main():
    global args
    p = argparse.ArgumentParser()
    p.add_argument("--port", type=int, required=True)
    p.add_argument("--delay-ms", type=int, default=0)
    p.add_argument("--marker", required=True)
    p.add_argument("--repo", default="", help="comma-separated repositories this mirror holds; empty means all")
    p.add_argument("--log", required=True)
    args = p.parse_args()

    if os.path.exists(args.log):
        os.remove(args.log)

    srv = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    print("fake mirror %s on %d (delay %dms)" % (args.marker, args.port, args.delay_ms), flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    sys.exit(main())
