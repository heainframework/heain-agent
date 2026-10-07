"""A small client for heain-agent's local API (Python standard library only).

The agent starts this app with HEAIN_AGENT_URL and HEAIN_AGENT_TOKEN set.
Calls made while handling a job or a request pass that request's id
(the X-Heain-Request header the agent sent), so they run in its trace,
lane and reasoning-record context.
"""
import base64
import json
import os
import urllib.error
import urllib.request


class AgentError(Exception):
    def __init__(self, status, code, message):
        super().__init__(f"{status} {code}: {message}")
        self.status, self.code, self.message = status, code, message


class Agent:
    def __init__(self, url=None, token=None):
        self.url = (url or os.environ["HEAIN_AGENT_URL"]).rstrip("/")
        self.token = token or os.environ.get("HEAIN_AGENT_TOKEN") or open(os.environ["HEAIN_AGENT_TOKEN_FILE"]).read().strip()

    def do(self, method, path, body=None, request=None):
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(self.url + path, data=data, method=method)
        req.add_header("Authorization", "Bearer " + self.token)
        if data is not None:
            req.add_header("Content-Type", "application/json")
        if request:
            req.add_header("X-Heain-Request", request)
        try:
            with urllib.request.urlopen(req, timeout=90) as r:
                return json.loads(r.read() or b"null")
        except urllib.error.HTTPError as e:
            try:
                err = json.loads(e.read()).get("error", {})
            except Exception:
                err = {}
            raise AgentError(e.code, err.get("code", ""), err.get("message", "")) from None

    # reasoning records (spec 04): input is hashed by the agent and never sent on
    def reason(self, request, capability, decision, input_bytes=b"", **kw):
        d = dict(capability=capability, decision=decision, input_b64=base64.b64encode(input_bytes).decode(), **kw)
        return self.do("POST", "/v1/reason", d, request)["record_id"]

    def mode(self):
        return self.do("GET", "/v1/mode")

    def journal(self, request, kind, data, lane=""):
        return self.do("POST", "/v1/journal", {"kind": kind, "lane": lane, "data": data}, request)

    def propose(self, request, type_, category, data=None, value=0, zone_id=""):
        return self.do("POST", "/v1/policy", {"type": type_, "category": category, "data": data or {}, "value": value, "zone_id": zone_id}, request)

    def policy(self, action_id):
        return self.do("GET", "/v1/policy/" + action_id)

    def broadcast(self, request, discovery_id, origin_zone, payload):
        return self.do("POST", "/v1/broadcast", {"discovery_id": discovery_id, "origin_zone": origin_zone, "payload": payload}, request)

    def call(self, request, app, capability, method, path, body=None):
        return self.do("POST", "/v1/call", {"app": app, "capability": capability, "method": method, "path": path, "body": body}, request)

    def submit(self, capability, payload, delivery="IMMEDIATE", **kw):
        return self.do("POST", "/v1/jobs", dict(capability=capability, payload_b64=base64.b64encode(payload).decode(),
                                                 classification={"delivery": delivery}, **kw))

    def seal(self, key, plaintext, aad=b""):
        r = self.do("POST", "/v1/seal", {"key": key, "plaintext_b64": base64.b64encode(plaintext).decode(), "aad_b64": base64.b64encode(aad).decode()})
        return base64.b64decode(r["sealed_b64"])

    def open(self, key, sealed, aad=b""):
        r = self.do("POST", "/v1/open", {"key": key, "sealed_b64": base64.b64encode(sealed).decode(), "aad_b64": base64.b64encode(aad).decode()})
        return base64.b64decode(r["plaintext_b64"])
