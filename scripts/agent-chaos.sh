#!/usr/bin/env bash
# THE FULL CLAIM: the AGENT (not just raw SQL) keeps working through a region failure, and
# its audit chain has zero gaps afterwards.
#
# chaos.sh proves the database survives. This proves the application on top of it survives —
# which is the actual submission claim. Run after ./scripts/cluster-up.sh.
set -euo pipefail

BIN=${BIN:-/tmp/amnesia-agent}
hr() { printf '\n\033[1;33m%s\033[0m\n' "=== $* ==="; }

command -v go >/dev/null && go build -o "$BIN" ./cmd/agent

hr "0. BASELINE — agent learns from experience, all 3 regions up"
"$BIN" demo | tail -12

hr "1. CHAIN BEFORE THE OUTAGE"
"$BIN" verify

hr "2. KILL REGION eu-west-1"
docker kill roach3 >/dev/null
# is_live lags ~9-14s behind the kill until the liveness lease expires; reading sooner shows
# a node that looks alive and makes the receipt contradict itself.
echo -n "waiting 16s for the liveness lease to expire"
for _ in $(seq 1 16); do sleep 1; echo -n .; done; echo
docker exec roach1 ./cockroach node status --insecure --format=tsv 2>/dev/null \
  | awk 'BEGIN{OFS="\t"} NR==1{print "id","address","is_available","is_live"} NR>1{print $1,$2,$(NF-1),$NF}'

hr "3. THE AGENT HANDLES A KNOWN INCIDENT WITH A REGION DEAD"
# Deliberately the SAME incident the agent learned about in step 0. The claim under test is
# that consolidated memory is still reachable while a third of the cluster is gone -- so the
# signal text must match what it learned, otherwise we would only be proving that an
# unfamiliar incident falls back to the playbook (true, but not the point).
"$BIN" -agent agent_a1 recall "disk_pressure: wal segments piling up on roach3"
echo
curl -s -m 25 -X POST "localhost:8791/api/incident?kind=disk_pressure&detail=wal+segments+piling+up+on+roach3" \
  | python3 -m json.tool || echo "(serve not running: start it with '$BIN serve &')"

hr "4. CHAIN INTEGRITY DURING THE OUTAGE"
# The claim that matters: not one link lost, duplicated, or reordered while a region was gone.
"$BIN" verify

hr "5. RESTORE THE REGION"
docker start roach3 >/dev/null
echo -n "waiting for roach3 to rejoin and catch up"
for _ in $(seq 1 20); do sleep 1; echo -n .; done; echo

hr "6. THE REVIVED NODE SERVES BACK WHAT IT NEVER SAW"
# Executed INSIDE the container that was killed. It was not running when these rows were
# written, so serving them proves Raft replicated and self-healed.
docker exec -i roach3 ./cockroach sql --insecure -d agentmem \
  -e "SELECT substring(content,1,64) AS content, crdb_region, created_at
      FROM memories WHERE agent_id='agent_a1'
      ORDER BY created_at DESC LIMIT 3;"

hr "7. FINAL CHAIN VERIFICATION"
"$BIN" verify
docker exec roach1 ./cockroach node status --insecure --format=tsv 2>/dev/null \
  | awk 'BEGIN{OFS="\t"} NR==1{print "id","address","is_available","is_live"} NR>1{print $1,$2,$(NF-1),$NF}'
