# Amnesia-Proof Agent

**An AI agent whose memory survives losing an entire cloud region.**

Built for the [CockroachDB × AWS Hackathon — Build with Agentic Memory](https://cockroachdb-ai.devpost.com/).

> *"An agent whose memory goes offline doesn't degrade gracefully, it stops."*

Most agent memory is a vector store bolted onto an app. Kill its availability zone and the
agent hits amnesia mid-task: it forgets what it was doing, re-runs side effects, or halts.
This project puts **every memory tier inside CockroachDB** and then kills a region on camera
to prove the agent keeps thinking.

```
=== 2. KILL REGION eu-west-1 ===
id  address        is_available  is_live
1   roach1:26257   true          true
2   roach2:26257   true          true
3   roach3:26257   false         false     ← an entire region is gone

=== 3. THE AGENT HANDLES A KNOWN INCIDENT WITH A REGION DEAD ===
{
    "action": "truncate WAL and expand the volume before paging",
    "from_memory": true,
    "rationale": "recalled lesson (dist 0.0000, region us-east-1): runbook: ..."
}

=== 4. CHAIN INTEGRITY DURING THE OUTAGE ===
chain VERIFIED: 14 links, 0 gaps, 0 bad hashes, 0 broken links
```

Full captured output: [`receipts/`](receipts/).

---

## Reproduce the whole thing in two commands

```bash
./scripts/cluster-up.sh      # 3-node, 3-region CockroachDB + schema
./scripts/agent-chaos.sh     # agent learns, region dies, agent keeps working
```

No cloud account and no API keys needed. Takes about 90 seconds.

---

## Memory tiers → CockroachDB primitives

The design claim is that agent memory is not one thing, and each tier maps to a database
primitive that a bolt-on vector store simply does not have.

| Tier | Primitive | Why it can't be faked elsewhere |
|---|---|---|
| **Working state** | serializable transactions (default) | state + embedding + audit row commit atomically — no window where they disagree |
| **Semantic recall** | `VECTOR INDEX (agent_id, embedding)` — C-SPANN | per-agent scoped ANN, proven in the query plan |
| **Forgetting** | **Row-Level TTL** (`ttl_expiration_expression`) | decay as a batched, admission-controlled DB job — not a cron sweeping rows |
| **Belief replay** | **`AS OF SYSTEM TIME`** | *"what did the agent believe when it acted?"* — bounded by `gc.ttlseconds` (4h default) |
| **Survivability** | `REGIONAL BY ROW` + `SURVIVE REGION FAILURE` | the receipt above |
| **Isolation** | **Row-Level Security** | tenant separation enforced by the DB, not a `WHERE` clause someone forgets |
| **Integrity** | hash-chained HMAC receipts, same txn | zero gaps across an outage, verifiable in one command |

### Consolidation: the agent gets better with experience

Episodic memories decay after 24h. After three similar incidents the agent **consolidates**
them into a durable semantic lesson that never expires — and from then on acts on its own
experience instead of its innate playbook:

```
encounter #1  → page a human                                  (innate playbook)
encounter #2  → page a human                                  (innate playbook)
encounter #3  → page a human    *** consolidated a lesson ***
encounter #4  → truncate WAL and expand the volume before paging   ← FROM MEMORY
```

Consolidation is idempotent — the lesson is written once, not re-learned on every encounter.

---

## Hackathon requirements

**CockroachDB tools — all four of the four named:**

| Tool | Where |
|---|---|
| Distributed Vector Indexing | `sql/schema.sql`, `internal/memory/store.go` — plan verified via `agent explain` |
| Managed MCP Server | `internal/mcp/` — audited read path against `cockroachlabs.cloud/mcp` |
| ccloud CLI | `scripts/ccloud-provision.sh` — provisions the multi-region cloud cluster |
| Agent Skills Repo | `skills/` — consolidated lessons emitted in the sponsor's own format |

**AWS service:** S3 (audit-chain export, `internal/awsexport/`) + Lambda entry point
(`lambda/`). Deliberately **not** Bedrock in the critical path — per-model access approval can
take days, and nothing that gates the deadline should sit in someone else's queue. Bedrock is
supported as an optional embedding upgrade that nothing depends on.

**Live demo:** **https://deploytest.theodoikenh.com/amnesia/** — dashboard, `/healthz`, `/api/cluster`,
`/api/verify`, and an SSE stream. Backed by a real 3-node/3-region CockroachDB cluster on the host.
`/api/cluster`, `/api/verify` and an SSE stream.

---

## Two clusters, and why

The resilience demo runs on **local Docker**, not CockroachDB Cloud, and this is a deliberate,
disclosed choice rather than a shortcut:

> **You cannot kill a node in CockroachDB Cloud Basic/Standard.** There is no DB Console, and
> `crdb_internal` is restricted by default (`SQLSTATE 42501`). Proving survival requires
> killing a node, so the chaos cluster has to be one we control.

The Cloud cluster is used for the **managed MCP Server** and the multi-region topology. Two
clusters, two purposes, stated plainly rather than blurred.

---

## Commands

```bash
agent demo             # the full learning arc, then verify the chain
agent explain          # print the query plan; fails loudly if the vector index is not used
agent recall "<text>"  # semantic recall with distances
agent verify           # re-verify the hash chain (run this AFTER a region failure)
agent belief 60s       # what the agent knew a minute ago (AS OF SYSTEM TIME)
agent export           # ship the audit chain to S3 (dry-runs without credentials)
agent serve            # dashboard + SSE + JSON API
```

---

## Four traps we hit, verified live on v26.2.4

These cost real debugging time and none of them are documented. Each is commented at the
site in the code.

1. **`REGIONAL BY ROW` silently makes `crdb_region` the *first* prefix column of the vector
   index.** So the natural reading of "constrain every prefix column to an equality" —
   *also pin the region* — is exactly wrong and **costs you the index**:

   | Predicate | Plan |
   |---|---|
   | `WHERE agent_id = $1` | ✅ `• vector search ... mem_agent_id_embedding_idx` |
   | `WHERE agent_id = $1 AND crdb_region = $2` | ❌ `• scan ... memories@pk_memories` |

2. **A `kind = 'semantic'` filter defeats the index the same way** (`• filter → • scan`).
   `RecallLesson` over-fetches and filters in Go instead. This also fixes a real bug: as
   episodic memories accumulate they crowd the one consolidated lesson out of a small top-k,
   and the agent silently stops using what it learned.

3. **A subquery-built vector defeats the index** where an identical literal does not. Pass
   embeddings as bound parameters, never as a subquery.

4. **Grammar + timing:** `WITH (ttl_expiration_expression = …)` must come *before*
   `LOCALITY REGIONAL BY ROW` (`SQLSTATE 42601` otherwise), and `is_live` lags **~14s**
   behind a `docker kill` until the liveness lease expires — read the node table too early
   and your own receipt contradicts you.

---

## Honest limitations

- **Time travel is bounded** by `gc.ttlseconds` (4h default), not unlimited.
- **Embeddings are a local hashed bag-of-words**, not a neural model. Deterministic on
  purpose: `chaos.sh` produces identical distances on every run, which is what makes the
  receipts auditable. Similarity here is lexical.
- **The remediation is simulated.** This is judged on the memory layer; an agent claiming to
  have restarted a production node would be an unverifiable claim of exactly the kind the
  Production Readiness criterion should punish.
- **Vector indexes:** create before loading (backfill blocks writes), don't batch vector
  inserts, `IMPORT INTO` unsupported.

## Layout

```
cmd/agent/           CLI + HTTP/SSE server
internal/memory/     memory tiers, vector recall, consolidation
internal/agent/      sense → retrieve → decide → act → persist
internal/receipt/    hash-chained audit log
internal/awsexport/  S3 export (hand-rolled SigV4, no SDK)
sql/schema.sql       every primitive, commented
scripts/             cluster-up · chaos · agent-chaos
receipts/            captured proof from real runs
```

## License

Apache-2.0 — see [LICENSE](LICENSE).
