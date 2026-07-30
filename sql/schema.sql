-- Amnesia-Proof Agent — memory schema
--
-- Every memory tier maps to a distinct CockroachDB primitive. That mapping is the point of
-- the submission: a pgvector table can hold embeddings, but it cannot survive a region loss,
-- decay rows as a first-class DB operation, or answer "what did the agent believe at 14:03?".
--
-- VERIFIED on v26.2.4 (3-node/3-region Docker, 2026-07-30):
--   * vector indexes are DEFAULT-ON. The v25.2 docs telling you to set
--     `feature.vector_index.enabled` are stale.
--   * SURVIVE REGION FAILURE + REGIONAL BY ROW work on CORE — no enterprise license.
--   * the planner really uses the C-SPANN index:
--       • vector search  table: mem@mem_agent_id_embedding_idx  prefix spans: [/'a1' - /'a1']

CREATE DATABASE IF NOT EXISTS agentmem;

-- Multi-region topology. SURVIVE REGION FAILURE needs >=3 regions and is what makes the
-- killed-region receipt possible: quorum is retained when one region disappears.
ALTER DATABASE agentmem SET PRIMARY REGION 'us-east-1';
ALTER DATABASE agentmem ADD REGION IF NOT EXISTS 'us-west-2';
ALTER DATABASE agentmem ADD REGION IF NOT EXISTS 'eu-west-1';
ALTER DATABASE agentmem SURVIVE REGION FAILURE;

USE agentmem;

-- Rangefeeds power CHANGEFEEDs (stretch goal: stream memory mutations to an observability sink).
SET CLUSTER SETTING kv.rangefeed.enabled = true;

-------------------------------------------------------------------------------
-- TIER 2: semantic memory (vector recall)
-------------------------------------------------------------------------------
-- PITFALL: the vector index is only used when EVERY prefix column is constrained to an
-- equality. `WHERE agent_id = 'a1'` uses the index; `WHERE tenant_id >= 5` silently falls
-- back to a full scan. So the prefix is agent_id, which every query pins by definition.
--
-- PITFALL: create the index BEFORE loading rows. On a non-empty table the backfill blocks
-- writes and needs sql_safe_updates=false. Declaring it inline at CREATE TABLE avoids this.
CREATE TABLE IF NOT EXISTS memories (
    id          UUID        NOT NULL DEFAULT gen_random_uuid(),
    agent_id    STRING      NOT NULL,
    kind        STRING      NOT NULL DEFAULT 'episodic',  -- episodic | semantic | checkpoint
    content     STRING      NOT NULL,
    embedding   VECTOR(384) NOT NULL,
    importance  FLOAT       NOT NULL DEFAULT 0.5,
    -- Decay horizon. NULL = never forget (consolidated/semantic memories).
    expires_at  TIMESTAMPTZ NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    task_id     STRING      NULL,
    CONSTRAINT pk_memories PRIMARY KEY (agent_id, id),
    VECTOR INDEX mem_agent_id_embedding_idx (agent_id, embedding)
)
-- TIER 3: forgetting as a DB primitive. Row-Level TTL deletes in batches at low
-- admission-control priority under the elastic CPU limiter, and checkpoints across restarts.
-- ttl_expiration_expression (not ttl_expire_after) lets each row carry its own horizon, so
-- importance drives retention instead of one global age cutoff.
--
-- GRAMMAR PITFALL (hit live on v26.2.4): WITH (...) must come BEFORE LOCALITY. Writing
-- `) LOCALITY REGIONAL BY ROW WITH (...)` fails with `at or near "with": syntax error`
-- SQLSTATE 42601. Verified working order is: column list -> WITH storage params -> LOCALITY.
WITH (
    ttl_expiration_expression = $$ COALESCE(expires_at, created_at + INTERVAL '100 years') $$,
    ttl_job_cron = '*/15 * * * *'
)
LOCALITY REGIONAL BY ROW;

-------------------------------------------------------------------------------
-- TIER 1: working state (transactional, same txn as the embedding)
-------------------------------------------------------------------------------
-- The sponsor's own pitch: "no consistency gaps between your vector data and your
-- operational database." State, embedding and audit row commit atomically or not at all.
CREATE TABLE IF NOT EXISTS tasks (
    id         STRING      NOT NULL PRIMARY KEY,
    agent_id   STRING      NOT NULL,
    goal       STRING      NOT NULL,
    step       INT         NOT NULL DEFAULT 0,
    status     STRING      NOT NULL DEFAULT 'running',  -- running | done | failed
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
) LOCALITY REGIONAL BY ROW;

-------------------------------------------------------------------------------
-- Audit: hash-chained receipts
-------------------------------------------------------------------------------
-- Each receipt commits in the SAME transaction as the memory write it attests to, so the
-- chain cannot contain a link for a write that rolled back. After a region failure we
-- re-verify the whole chain: "zero gaps in an N-link HMAC chain across a region outage" is
-- a one-line-verifiable cryptographic claim rather than a promise.
CREATE TABLE IF NOT EXISTS receipts (
    seq        INT         NOT NULL,
    agent_id   STRING      NOT NULL,
    memory_id  UUID        NULL,
    event      STRING      NOT NULL,
    prev_hash  STRING      NOT NULL,
    hash       STRING      NOT NULL,
    region     STRING      NOT NULL DEFAULT gateway_region(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT pk_receipts PRIMARY KEY (agent_id, seq)
) LOCALITY REGIONAL BY ROW;

-------------------------------------------------------------------------------
-- Isolation: Row-Level Security
-------------------------------------------------------------------------------
-- Multi-tenant agent memory: one agent must not read another's. Enforced by the database,
-- not by a WHERE clause the application might forget. Demo shot: root sees all rows,
-- agent_a1 sees only its own.
ALTER TABLE memories ENABLE ROW LEVEL SECURITY;
ALTER TABLE tasks    ENABLE ROW LEVEL SECURITY;

CREATE USER IF NOT EXISTS agent_a1;
CREATE USER IF NOT EXISTS agent_a2;
GRANT SELECT, INSERT, UPDATE ON memories TO agent_a1, agent_a2;
GRANT SELECT, INSERT, UPDATE ON tasks    TO agent_a1, agent_a2;

DROP POLICY IF EXISTS memories_own_rows ON memories;
CREATE POLICY memories_own_rows ON memories
    USING (agent_id = current_user);

DROP POLICY IF EXISTS tasks_own_rows ON tasks;
CREATE POLICY tasks_own_rows ON tasks
    USING (agent_id = current_user);
