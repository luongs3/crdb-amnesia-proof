---
name: for-disk-pressure-truncate-wal-and-expand-the-volume-before
description: >-
  Learned remediation for disk_pressure incidents, consolidated by the Amnesia-Proof Agent
  after repeated exposure. Emitted automatically from CockroachDB semantic memory.
domain: resilience-and-disaster-recovery
---

# Disk_pressure

## When this applies

A disk_pressure signal is observed on a CockroachDB cluster.

## What to do

runbook: for disk_pressure, truncate WAL and expand the volume before paging

## Why this is the remediation

This skill was not hand-written. The agent encountered disk_pressure incidents repeatedly and
initially fell back on its innate playbook ("page a human"). After three closely-related
episodes it consolidated them into a durable semantic memory, which now overrides the
default on every subsequent encounter.

## Provenance

- Consolidated from episodic memory in CockroachDB (agentmem.memories)
- Memory id: a6bd70e2-5eb4-4375-a711-be3b056471b5
- Region: us-east-1
- Consolidated at: 2026-07-30T07:38:08Z
- Durability: semantic memories have no TTL expiry; the episodes behind them decay after 24h
- Every write attested by a hash-chained HMAC receipt committed in the same transaction

## Verify

    agent recall "disk_pressure"
    agent verify
