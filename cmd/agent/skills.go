package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/luongs3/crdb-amnesia-proof/internal/agent"
	"github.com/luongs3/crdb-amnesia-proof/internal/memory"
)

// emitSkills writes the agent's consolidated semantic memories out as SKILL.md files in
// CockroachDB's own Agent Skills format (the fourth named sponsor tool).
//
// This closes the loop the whole project is about: episodic experience -> consolidated
// semantic memory -> a portable, human-readable skill another agent can load. Memory that
// only lives in a vector table is trapped; memory that compiles to a skill is transferable.
//
// The sponsor's skills repo ships 34 skills but leaves four domains empty
// (performance-and-scaling, resilience-and-disaster-recovery, cost-and-usage-management,
// integrations-and-ecosystem) and has nothing at all on vectors, RAG or agentic memory. The
// skills emitted here target resilience-and-disaster-recovery.
func emitSkills(ctx context.Context, store *memory.Store) error {
	outDir := "skills"
	if v := os.Getenv("SKILLS_DIR"); v != "" {
		outDir = v
	}

	// Consolidated lessons are semantic memories. Pull a wide candidate set and filter in Go —
	// a `kind = 'semantic'` predicate would defeat the vector index (see memory package docs).
	probe := agent.HashEmbedder{}.Embed("runbook incident remediation")
	mems, err := store.Recall(ctx, probe, 200)
	if err != nil {
		return err
	}

	var lessons []memory.Memory
	for _, m := range mems {
		if m.Kind == memory.Semantic {
			lessons = append(lessons, m)
		}
	}
	if len(lessons) == 0 {
		return fmt.Errorf("no consolidated lessons yet — run `agent demo` first")
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	for _, l := range lessons {
		name := slug(l.Content)
		dir := filepath.Join(outDir, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		path := filepath.Join(dir, "SKILL.md")
		if err := os.WriteFile(path, []byte(renderSkill(l)), 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", path)
	}
	fmt.Printf("\n%d consolidated lesson(s) emitted as Agent Skills\n", len(lessons))
	return nil
}

func renderSkill(l memory.Memory) string {
	title := strings.TrimPrefix(l.Content, "runbook: ")
	incident := "an incident"
	if i := strings.Index(title, ", "); i > 0 {
		incident = strings.TrimPrefix(title[:i], "for ")
		title = title[i+2:]
	}

	return fmt.Sprintf(`---
name: %s
description: >-
  Learned remediation for %s incidents, consolidated by the Amnesia-Proof Agent
  after repeated exposure. Emitted automatically from CockroachDB semantic memory.
domain: resilience-and-disaster-recovery
---

# %s

## When this applies

A %s signal is observed on a CockroachDB cluster.

## What to do

%s

## Why this is the remediation

This skill was not hand-written. The agent encountered %s incidents repeatedly and
initially fell back on its innate playbook ("page a human"). After three closely-related
episodes it consolidated them into a durable semantic memory, which now overrides the
default on every subsequent encounter.

## Provenance

- Consolidated from episodic memory in CockroachDB (%s)
- Memory id: %s
- Region: %s
- Consolidated at: %s
- Durability: semantic memories have no TTL expiry; the episodes behind them decay after 24h
- Every write attested by a hash-chained HMAC receipt committed in the same transaction

## Verify

    agent recall "%s"
    agent verify
`,
		slug(l.Content), incident, strings.ToUpper(incident[:1])+incident[1:],
		incident, l.Content, incident,
		"agentmem.memories", l.ID, l.Region,
		l.CreatedAt.UTC().Format(time.RFC3339), incident)
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.TrimPrefix(strings.ToLower(s), "runbook: ")
	s = nonAlnum.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 60 {
		s = strings.Trim(s[:60], "-")
	}
	return s
}
