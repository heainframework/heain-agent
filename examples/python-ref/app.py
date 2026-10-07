"""agent-ref: the conformance reference app (heain-sdk's refapp) in Python,
through heain-agent. Standard library only.

The agent claims jobs and POSTs them here (/jobs/<capability>), and forwards
every declared direct endpoint here after its own mTLS, lane, user and
offline checks; it audits formal processes and requires a reasoning record
for each AI capability.
"""
import base64
import hashlib
import json
import os
import re
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from heain_agent import Agent

MODEL_SHA256 = hashlib.sha256(b"ref-rules v1: loud if >= 2 '!'").hexdigest()
agent = Agent()


def rate(text):
    return ("loud", 0.9) if text.count("!") >= 2 else ("calm", 0.7)


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        pass

    def body(self):
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n else b""
        try:
            return json.loads(raw or b"{}")
        except ValueError:
            return {}

    def reply(self, obj, status=200):
        obj = dict(obj)
        obj.setdefault("trace", self.headers.get("X-Heain-Trace", ""))
        raw = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def fail(self, err):
        self.reply({"error": str(err)}, 502)

    def do_GET(self):
        req = self.headers.get("X-Heain-Request")
        if self.path == "/ready":
            return self.reply({"ready": True})
        m = re.fullmatch(r"/v1/greet/([^/?]+)", self.path)
        if m:
            return self.reply({"greeting": "hi " + m.group(1)})
        m = re.fullmatch(r"/v1/propose/([^/?]+)", self.path)
        if m:
            try:
                r = agent.policy(m.group(1))
            except Exception as e:
                return self.fail(e)
            return self.reply({"action_id": r["action_id"], "result": r["result"]})
        self.reply({"error": "not found"}, 404)

    def do_POST(self):
        req = self.headers.get("X-Heain-Request")
        b = self.body()
        try:
            if self.path == "/v1/greet":
                if agent.mode().get("mode") == "standalone":
                    agent.journal(req, "greet.served", {"standalone": True})  # spec 02 §9
                return self.reply({"greeting": "hello " + str(b.get("name", "")), "by": self.headers.get("X-Heain-Caller", "")})
            if self.path == "/v1/score":
                text = str(b.get("text", ""))
                tone, conf = rate(text)
                if "no-reasoning" in text:  # for the agent's live test: an AI answer without its record
                    return self.reply({"tone": tone})
                rid = agent.reason(req, "ref.score", tone, text.encode(), confidence=conf, summary="counted exclamation marks",
                                   role="decision", model_sha256=MODEL_SHA256, factors=[{"name": "exclamations", "value": text.count("!")}])
                return self.reply({"tone": tone, "record_id": rid})
            if self.path == "/v1/id":
                return self.reply({"verified": True})
            if self.path == "/v1/ballot":
                return self.reply({"cast": True})
            if self.path == "/v1/propose":
                r = agent.propose(req, "ref.grant.extend", "ALLOWLIST_BASED", {"grant": "g1"})
                return self.reply({"action_id": r["action_id"], "result": r["result"]})
            if self.path == "/v1/broadcast":
                r = agent.broadcast(req, uuid.uuid4().hex[:8], "zone-a", {"rule": "ref-lighting-adjust", "customer_id": "cust-42"})
                return self.reply({"action_id": r["action_id"], "result": r["result"]})
            if self.path == "/v1/relay":
                # a uses[] dependency, found through discovery by the agent -- no address here
                r = agent.call(req, "agent-ref", "ref.greet", "POST", "/v1/greet", {"name": "relayed " + str(b.get("name", ""))})
                return self.reply({"relayed": (r.get("body") or {}).get("greeting")})
            if self.path == "/jobs/ref.upper":
                payload = base64.b64decode(b.get("payload_b64") or "")
                return self.reply({"output_b64": base64.b64encode(payload.upper()).decode()})
            if self.path == "/jobs/ref.label":
                payload = base64.b64decode(b.get("payload_b64") or "")
                tone, conf = rate(payload.decode(errors="replace"))
                if b"no-reasoning" in payload:  # for the agent's live test: an AI job without its record
                    return self.reply({"output_b64": base64.b64encode(tone.encode()).decode()})
                return self.reply({"output_b64": base64.b64encode(tone.encode()).decode(),
                                   "reasoning": [{"decision": tone, "input_b64": b.get("payload_b64") or "", "confidence": conf,
                                                  "summary": "counted exclamation marks", "role": "advisory", "model_sha256": MODEL_SHA256}]})
        except Exception as e:
            return self.fail(e)
        self.reply({"error": "not found"}, 404)


if __name__ == "__main__":
    port = int(os.environ.get("HEAIN_APP_PORT", "18610"))
    srv = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    print(f"agent-ref (python): listening on 127.0.0.1:{port}", flush=True)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
