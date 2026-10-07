#!/usr/bin/env bash
# heain-agent live test 5d: a Python app (examples/python-ref, standard library only) on a real heain-core,
# through heain-agent.  G (18000, root, provisioning).
#  - the agent enrols, registers agent-ref (app.sdk heain-agent), starts the Python app as its child;
#  - the local API: loopback only, a token (0600) is required, an unknown X-Heain-Request is refused;
#  - direct endpoints: another app calls over mTLS, the agent forwards to Python; formal calls audited;
#    an AI answer comes with its reasoning record, and one without it is refused (reasoning_record_missing);
#  - jobs: the app submits through the agent, the agent claims and POSTs them to Python; an AI job without
#    its record fails (not retried); the app seals data with a key that never leaves the agent;
#  - the app exits -> the agent stops and leaves; SIGTERM to the agent stops the app.
# Needs ~/heain-core, ~/heain-sdk. ~2 min.  Run from ~/heain-agent:  bash scripts/live_5d.sh
set -uo pipefail
AG=$(cd "$(dirname "$0")/.." && pwd)
cd ~/heain-core || { echo "needs ~/heain-core"; exit 1; }
H=./test_1_2_live.sh
T=$HOME/heain-core/.test-1.2
C=$T/certs; L=$T/logs; P=$T/pids; BIN=$T/node; W=$T/agent-5d
URL=https://127.0.0.1:18000; EP=https://127.0.0.1:19620
PASS=0; FAIL=0
ok()  { echo "  PASS: $*"; PASS=$((PASS+1)); }
bad() { echo "  FAIL: $*"; FAIL=$((FAIL+1)); }
as() { local who=$1; shift; curl -sk --noproxy '*' --cert "$C/$who.pem" --key "$C/$who.key" --cacert "$C/ca.pem" "$@"; }
code() { local who=$1; shift; as "$who" -o /dev/null -w "%{http_code}" "$@"; }
cl() { local who=$1; shift; curl -sk --noproxy '*' --cert "$W/$who.pem" --key "$W/$who.key" --cacert "$C/ca.pem" -H 'Content-Type: application/json' "$@"; }
j() { python3 -c "import json,sys;d=json.load(sys.stdin);print($1)" 2>/dev/null; }
mkcert() { [ -f "$C/$1.pem" ] && return; openssl genrsa -out "$C/$1.key" 2048 >/dev/null 2>&1
  openssl req -new -key "$C/$1.key" -subj "/CN=$1" -out "$C/$1.csr" >/dev/null 2>&1
  openssl x509 -req -in "$C/$1.csr" -CA "$C/ca.pem" -CAkey "$C/ca.key" -CAcreateserial -out "$C/$1.pem" \
    -days 825 -sha256 -extfile <(printf "subjectAltName=DNS:%s" "$1") >/dev/null 2>&1; }
token() { as admin -X POST -H 'Content-Type: application/json' -d "{\"label\":\"$1\"}" $URL/provision/token > "$W/$2.tok"; }
pending() { as approver-1 $URL/v1/admin/policy/pending | j "' '.join(x['ID'] for x in d['actions'] if x['Type']=='$1')"; }
approve() { code approver-1 -X POST "$URL/v1/admin/policy/$1/approve"; }
audit() { as admin "$URL/v1/admin/audit?limit=5000&from=1"; }
client() { as admin -X POST -H 'Content-Type: application/json' -d "{\"label\":\"$1\"}" $URL/provision/token > "$W/$1.json"
  python3 - "$W" "$1" <<'PY'
import json,sys; w,l=sys.argv[1],sys.argv[2]; d=json.load(open(f"{w}/{l}.json"))
open(f"{w}/{l}.boot.pem","w").write(d["bootstrap_cert_pem"]+open(w+"/prov.pem").read()); open(f"{w}/{l}.boot.key","w").write(d["bootstrap_key_pem"]); open(f"{w}/{l}.token","w").write(d["token"])
PY
  openssl genrsa -out "$W/$1.key" 2048 >/dev/null 2>&1; openssl req -new -key "$W/$1.key" -subj "/CN=$1" -out "$W/$1.csr" >/dev/null 2>&1
  python3 -c "import json;print(json.dumps({'token':open('$W/$1.token').read(),'csr_pem':open('$W/$1.csr').read()}))" > "$W/$1.req"
  curl -sk --noproxy '*' --cert "$W/$1.boot.pem" --key "$W/$1.boot.key" --cacert "$C/ca.pem" -X POST -H 'Content-Type: application/json' -d @"$W/$1.req" $URL/provision/csr \
    | python3 -c "import json,sys;open('$W/$1.pem','w').write(json.load(sys.stdin)['cert_pem']+open('$W/prov.pem').read())"; }
# the agent's local API, as the app: lc <method> <path> [json] [request-id]
LOC=""; TOK=""
lc() { curl -s --noproxy '*' -X "$1" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' ${4:+-H "X-Heain-Request: $4"} ${3:+-d "$3"} "$LOC$2"; }
lcode() { curl -s --noproxy '*' -o /dev/null -w "%{http_code}" -X "$1" ${3:+-H "Authorization: Bearer $3"} "$LOC$2"; }
startagent() { # <instance> <config>
  mkdir -p "$W/state-$1"
  ( cd "$AG/examples/python-ref" || exit 1; HEAIN_AGENT_CONFIG=$2 HEAIN_MANIFEST=$AG/examples/python-ref/heain-app.yaml HEAIN_INSTANCE=$1 HEAIN_CORE_URL=$URL HEAIN_CORE_ID=G \
    HEAIN_CA=$C/ca.pem HEAIN_CHAIN=$W/prov.pem HEAIN_STATE_DIR=$W/state-$1 HEAIN_ENROLL_TOKEN=$W/$1.tok \
    HEAIN_ENDPOINT_BASE=$EP HEAIN_LISTEN=127.0.0.1:19620 nohup "$W/heain-agent" >> "$W/$1.log" 2>&1 & echo $! > "$P/agent-$1.pid" ); }

stopagents() { for f in "$P"/agent-*.pid; do [ -f "$f" ] && kill "$(cat "$f")" 2>/dev/null; rm -f "$f"; done
  for c in $(grep -ho "started the app .*(pid [0-9]*)" "$W"/*.log 2>/dev/null | grep -o "[0-9]*)$" | tr -d ')'); do kill "$c" 2>/dev/null; done; true; }
trap stopagents EXIT

echo "== 0. G; build"
$H clean >/dev/null; $H build >/dev/null || { echo "core build failed"; exit 1; }; $H certs >/dev/null
mkdir -p "$L" "$P" "$T/data-G" "$W"; for c in admin approver-1; do mkcert "$c"; done
openssl genrsa -out "$W/prov.key" 2048 >/dev/null 2>&1
openssl req -new -key "$W/prov.key" -subj "/CN=heain-test-provisioning-ca" -out "$W/prov.csr" >/dev/null 2>&1
openssl x509 -req -in "$W/prov.csr" -CA "$C/ca.pem" -CAkey "$C/ca.key" -CAcreateserial -out "$W/prov.pem" -days 30 -sha256 \
  -extfile <(printf "basicConstraints=critical,CA:TRUE\nkeyUsage=critical,keyCertSign,cRLSign") >/dev/null 2>&1
: > "$L/G.log"
nohup "$BIN" -node-id=G -tier=GLOBAL_PRIMARY -raft-addr=127.0.0.1:19000 -data-dir="$T/data-G" -http-addr=127.0.0.1:18000 -cert="$C/G.pem" -key="$C/G.key" \
  -ca="$C/ca.pem" -admin-node-id=admin -approver-ids=approver-1 -bootstrap=true -approval-store-path="$T/data-G/approvals.db" \
  -ingest-queue-path="$T/data-G/queue.db" -staging-path="$T/data-G/staging.db" -provision-ca-cert="$W/prov.pem" -provision-ca-key="$W/prov.key" >> "$L/G.log" 2>&1 &
echo $! > "$P/G.pid"; sleep 4
( cd "$AG" && GOFLAGS= GOWORK=${SDK_GOWORK:-} go build -o "$W/heain-agent" ./cmd/heain-agent ) && ok "heain-agent builds" || { bad "build"; exit 1; }
for k in $(seq 1 15); do [ "$(code admin -X POST -H 'Content-Type: application/json' -d '{"label":"probe.x"}' $URL/provision/token)" = 200 ] && break; sleep 1; done
client ops.c1

echo "== 1. the local API must be on loopback"
printf '{"listen":"0.0.0.0:7601","app_url":"http://127.0.0.1:0","command":["python3","app.py"]}' > "$W/bad.json"
out=$(cd "$AG/examples/python-ref" && HEAIN_AGENT_CONFIG=$W/bad.json timeout 10 "$W/heain-agent" 2>&1); rc=$?
[ $rc = 2 ] && echo "$out" | grep -q "must be a loopback address" && ok "a non-loopback local API is refused before anything starts" || bad "loopback: $rc $out"

echo "== 2. the agent registers agent-ref and starts the Python app"
token agent-ref.p1 p1
python3 -c "import json;d=json.load(open('$AG/examples/python-ref/heain-agent.json'));d['listen']='127.0.0.1:19621';d['token_file']='$W/p1.agent-token';json.dump(d,open('$W/p1.json','w'))"
startagent p1 "$W/p1.json"
for k in $(seq 1 60); do for a in $(pending app.register); do approve $a >/dev/null; done; grep -q ": active" "$W/p1.log" 2>/dev/null && break; sleep 1; done
grep -q ": active" "$W/p1.log" && grep -q "agent-ref (python): listening" "$W/p1.log" && ok "agent-ref admitted (manifest app.sdk heain-agent); the Python app runs as the agent's child" || { bad "start: $(tail -5 "$W/p1.log")"; $H stop-all >/dev/null 2>&1; exit 1; }
LOC=http://127.0.0.1:19621; TOK=$(cat "$W/p1.agent-token")
[ "$(stat -c %a "$W/p1.agent-token")" = 600 ] && [ ${#TOK} = 64 ] && ok "the local API token is written 0600" || bad "token file"
[ "$(lcode GET /v1/status)$(lcode GET /v1/status wrong)" = 401401 ] && [ "$(lc GET /v1/status | j "(d['app'], d['instance'])")" = "('agent-ref', 'p1')" ] \
  && ok "the local API refuses a call without the token or with a wrong one" || bad "token: $(lc GET /v1/status)"
[ "$(lc POST /v1/reason '{"capability":"ref.score","decision":"x"}' no-such-request | j "d['error']['code']")" = unknown_request ] && ok "a call naming an unknown X-Heain-Request is refused" || bad "unknown request"

echo "== 3. direct endpoints, through the agent's mTLS"
r=$(cl ops.c1 -X POST -d '{"name":"ann"}' -D "$W/h1" $EP/v1/greet)
[ "$(echo "$r" | j "d['greeting']")" = "hello ann" ] && [ "$(echo "$r" | j "d['by']")" = ops.c1 ] && grep -qi '^x-heain-trace:' "$W/h1" \
  && ok "another app calls POST /v1/greet over mTLS: Python answers, with the caller and the trace" || bad "greet: $r"
[ "$(curl -sk --noproxy '*' -o /dev/null -w '%{http_code}' -X POST $EP/v1/greet)" = 000 ] && ok "no client certificate: refused by the agent's TLS" || bad "no cert"
r=$(cl ops.c1 -X POST -d '{"text":"what a day!!"}' $EP/v1/score); RID=$(echo "$r" | j "d['record_id']")
[ "$(echo "$r" | j "d['tone']")" = loud ] && [ -n "$RID" ] && ok "POST /v1/score: the Python app sent its reasoning record through the agent ($RID)" || bad "score: $r"
r=$(cl ops.c1 -X POST -d '{"text":"no-reasoning"}' -o /dev/null -w "%{http_code}" $EP/v1/score)
[ "$r" = 500 ] && ok "an AI answer without its reasoning record is refused (reasoning_record_missing)" || bad "missing record: $r"
sleep 1; a=$(audit)
[ "$(echo "$a" | j "sum(1 for x in d['records'] if (x['event'].get('Detail') or {}).get('detail',{}).get('path')=='/v1/greet' and x['event']['Result']=='ok')")" = 1 ] \
  && [ "$(echo "$a" | j "any('$RID' in ((x['event'].get('Detail') or {}).get('detail',{}).get('reasoning_record_ids') or []) for x in d['records'])")" = True ] \
  && [ "$(echo "$a" | j "sum(1 for x in d['records'] if x['event']['Result']=='error:reasoning_record_missing')")" = 1 ] \
  && ok "core's audit: the formal call once, the score with its record, the refused one" || bad "audit: $(echo "$a" | j "[(x['event']['Action'],x['event']['Result']) for x in d['records']][-8:]")"
r=$(cl ops.c1 -X POST -d '{"name":"x"}' $EP/v1/relay)
[ "$(echo "$r" | j "d['relayed']")" = "hello relayed x" ] && ok "POST /v1/relay: Python calls ref.greet through the agent (discovery + mTLS)" || bad "relay: $r"

echo "== 4. jobs"
S1=$(lc POST /v1/jobs "{\"capability\":\"ref.upper\",\"payload_b64\":\"$(printf 'job one' | base64)\",\"classification\":{\"delivery\":\"IMMEDIATE\"}}"); T1=$(echo "$S1" | j "d['ticket_id']")
r=$(lc POST "/v1/jobs/$T1/wait?timeout_s=40")
[ "$(echo "$r" | j "(d['state'] in ('completed','delivered'), __import__('base64').b64decode(d.get('output_b64') or '').decode())")" = "(True, 'JOB ONE')" ] \
  && ok "the app submits a job through the agent; the agent claims it and POSTs it to Python; output JOB ONE" || bad "job: $S1 / $r"
T2=$(lc POST /v1/jobs "{\"capability\":\"ref.label\",\"payload_b64\":\"$(printf 'hey!!' | base64)\",\"classification\":{\"delivery\":\"IMMEDIATE\"}}" | j "d['ticket_id']")
r=$(lc POST "/v1/jobs/$T2/wait?timeout_s=40")
[ "$(echo "$r" | j "d['state'] in ('completed','delivered')")" = True ] && [ "$(audit | j "any((x['event'].get('Detail') or {}).get('detail',{}).get('ticket_id')=='$T2' and (x['event'].get('Detail') or {}).get('detail',{}).get('reasoning_record_ids') for x in d['records'])")" = True ] \
  && ok "an AI job: Python returns its reasoning with the output; the record is linked in the audit" || bad "ai job: $r"
T3=$(lc POST /v1/jobs "{\"capability\":\"ref.label\",\"payload_b64\":\"$(printf 'no-reasoning' | base64)\",\"classification\":{\"delivery\":\"IMMEDIATE\"}}" | j "d['ticket_id']")
sleep 8; r=$(lc GET "/v1/jobs/$T3")
[ "$(echo "$r" | j "d['state'] in ('completed','delivered')")" = False ] && [ "$(audit | j "any((x['event'].get('Detail') or {}).get('detail',{}).get('ticket_id')=='$T3' and x['event']['Result']=='error:reasoning_record_missing' for x in d['records'])")" = True ] \
  && ok "an AI job without its reasoning record fails (not retried), audited" || bad "ai job without record: $r"
S=$(lc POST /v1/seal "{\"key\":\"notes\",\"plaintext_b64\":\"$(printf 'secret note' | base64)\"}" | j "d['sealed_b64']")
[ -n "$S" ] && [ "$(lc POST /v1/open "{\"key\":\"notes\",\"sealed_b64\":\"$S\"}" | j "__import__('base64').b64decode(d['plaintext_b64']).decode()")" = "secret note" ] \
  && ! grep -rq "secret note" "$W/state-p1" && ok "seal / open with a data key that never leaves the agent; nothing in plaintext on disk" || bad "seal"

echo "== 5. lifecycles"
kill "$(grep -o "started the app .*(pid [0-9]*)" "$W/p1.log" | tail -1 | grep -o "[0-9]*)$" | tr -d ')')"; for k in $(seq 1 20); do kill -0 "$(cat "$P/agent-p1.pid")" 2>/dev/null || break; sleep 1; done
! kill -0 "$(cat "$P/agent-p1.pid")" 2>/dev/null && grep -q "the app exited" "$W/p1.log" && ok "the Python app exits -> the agent stops too" || bad "child exit: $(tail -3 "$W/p1.log")"
sleep 2; [ "$(as admin $URL/v1/admin/apps | j "[(i['status'], i['live']) for i in d['apps'] if i['instance_id']=='p1']")" != "[('active', True)]" ] && ok "and leaves core (no longer active)" || bad "still active: $(as admin $URL/v1/admin/apps | head -c 300)"
startagent p1 "$W/p1.json"
for k in $(seq 1 40); do [ "$(grep -c "agent-ref (python): listening" "$W/p1.log")" = 2 ] && break; sleep 1; done
CH=$(grep -o "started the app .*(pid [0-9]*)" "$W/p1.log" | tail -1 | grep -o "[0-9]*)$" | tr -d ')')
AP=$(cat "$P/agent-p1.pid"); kill -TERM "$AP"
# a zombie still answers kill -0, so look at the process state
alive() { st=$(ps -o stat= -p "$1" 2>/dev/null); [ -n "$st" ] && [ "${st#Z}" = "$st" ]; }
for k in $(seq 1 20); do alive "$CH" || alive "$AP" || break; sleep 1; done
[ -n "$CH" ] && ! alive "$CH" && ! alive "$AP" && ok "SIGTERM to the agent stops the Python app as well" || bad "child left running ($CH)"

echo "== cleanup"
stopagents
$H stop-all >/dev/null 2>&1
echo
echo "RESULT: $PASS passed, $FAIL failed"
echo "(logs: $W)"
