#!/usr/bin/env bash
# Stand up a 3-node / 3-region CockroachDB cluster for the Amnesia-Proof Agent demo.
#
# WHY DOCKER AND NOT COCKROACHDB CLOUD: you cannot kill a node in CockroachDB Cloud
# Basic/Standard — there is no DB Console and crdb_internal is restricted by default
# (SQLSTATE 42501). The region-failure receipt is the centrepiece of this submission, so
# the chaos cluster has to be one we control. The Cloud cluster is used for the managed
# MCP Server and the multi-region topology shots. Two clusters, two purposes.
set -euo pipefail

NET=crdbnet
IMG=cockroachdb/cockroach:latest
REGIONS=(us-east-1 us-west-2 eu-west-1)
# Host 8080 is occupied by the DataHub quickstart on this machine; 18080 is the console.
SQL_PORT=26257
CONSOLE_PORT=18080

log() { printf '\033[36m==>\033[0m %s\n' "$*"; }

log "tearing down any previous cluster"
for i in 1 2 3; do docker rm -f "roach$i" >/dev/null 2>&1 || true; done
docker network rm "$NET" >/dev/null 2>&1 || true

log "creating network $NET"
docker network create "$NET" >/dev/null

for i in 1 2 3; do
  region="${REGIONS[$((i-1))]}"
  ports=""
  [ "$i" -eq 1 ] && ports="-p ${SQL_PORT}:26257 -p ${CONSOLE_PORT}:8080"
  log "starting roach$i in region $region"
  # PITFALL: --locality MUST come after `start`. Placed before it, Docker treats it as a
  # docker flag and dies with `unknown flag: --locality`.
  # shellcheck disable=SC2086
  docker run -d --name="roach$i" --hostname="roach$i" --net="$NET" $ports "$IMG" \
    start --insecure \
    --locality="region=${region},zone=${region}a" \
    --join=roach1,roach2,roach3 >/dev/null
done

log "waiting for nodes to open their SQL ports"
sleep 5

log "initializing cluster"
docker exec roach1 ./cockroach init --insecure 2>/dev/null || log "already initialized"

log "waiting for all 3 nodes to report live"
for _ in $(seq 1 30); do
  live=$(docker exec roach1 ./cockroach node status --insecure --format=tsv 2>/dev/null \
         | awk 'NR>1 && $NF=="true"' | wc -l | tr -d ' ')
  [ "${live:-0}" -ge 3 ] && break
  sleep 2
done

log "applying schema"
docker exec -i roach1 ./cockroach sql --insecure < "$(dirname "$0")/../sql/schema.sql"

log "cluster up — console http://localhost:${CONSOLE_PORT}  sql postgres://root@localhost:${SQL_PORT}/agentmem?sslmode=disable"
docker exec roach1 ./cockroach node status --insecure --format=tsv \
  | awk 'BEGIN{OFS="\t"} NR==1{print "id","address","is_available","is_live"} NR>1{print $1,$2,$(NF-1),$NF}'
