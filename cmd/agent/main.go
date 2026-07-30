// Command agent exercises the Amnesia-Proof Agent's memory layer against CockroachDB.
//
//	agent demo      seed memories, run incidents, show the agent learning
//	agent explain   print the query plan (proves the vector index is used)
//	agent recall    semantic recall for an arbitrary query
//	agent verify    re-verify the hash chain (run this AFTER a region failure)
//	agent belief    what the agent knew N ago (AS OF SYSTEM TIME)
//	agent serve     HTTP + SSE dashboard, and the public demo URL
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"time"

	_ "github.com/lib/pq"

	"github.com/luongs3/crdb-amnesia-proof/internal/agent"
	"github.com/luongs3/crdb-amnesia-proof/internal/memory"
)

const defaultDSN = "postgres://root@localhost:26257/agentmem?sslmode=disable"

func main() {
	dsn := flag.String("dsn", envOr("CRDB_DSN", defaultDSN), "CockroachDB connection string")
	agentID := flag.String("agent", "agent_a1", "agent identity (also the RLS principal)")
	addr := flag.String("addr", envOr("ADDR", ":8791"), "listen address for `serve`")
	flag.Parse()

	cmd := flag.Arg(0)
	if cmd == "" {
		cmd = "demo"
	}

	db, err := sql.Open("postgres", *dsn)
	if err != nil {
		die("open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(8)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		die("cannot reach CockroachDB at %s: %v\n(run ./scripts/cluster-up.sh first)", *dsn, err)
	}

	// The HMAC key signs the audit chain. A fixed dev default keeps `verify` reproducible for
	// a judge cloning the repo; production would source this from a secret manager.
	key := []byte(envOr("RECEIPT_KEY", "amnesia-proof-dev-key"))
	store := memory.NewStore(db, *agentID, key)
	a := agent.New(db, *agentID, store, agent.HashEmbedder{})

	switch cmd {
	case "demo":
		run(demo(ctx, a, store))
	case "explain":
		run(explain(ctx, store))
	case "recall":
		run(recall(ctx, store, flag.Arg(1)))
	case "verify":
		run(verify(ctx, store, db))
	case "belief":
		run(belief(ctx, store, flag.Arg(1)))
	case "serve":
		run(serve(ctx, a, store, db, *addr))
	case "export":
		run(export(ctx, db, *agentID))
	case "mcp":
		run(mcpProbe(ctx))
	case "skills":
		run(emitSkills(ctx, store))
	default:
		die("unknown command %q (demo|explain|recall|verify|belief|serve|export|mcp|skills)", cmd)
	}
}

// demo drives the agent through the same incident three times, so the improvement from
// consolidated memory is visible rather than asserted.
func demo(ctx context.Context, a *agent.Agent, store *memory.Store) error {
	emb := agent.HashEmbedder{}

	fmt.Println("=== 1. seeding prior experience ===")
	seed := []struct {
		kind, text string
		imp        float64
	}{
		{"episodic", "disk pressure on roach3, wal segments piling up", 0.9},
		{"episodic", "replication lag spiked after the eu-west deploy", 0.7},
		{"episodic", "disk pressure again on roach3, wal not truncating", 0.8},
	}
	for _, s := range seed {
		id, err := store.Remember(ctx, memory.Memory{
			Kind: memory.Kind(s.kind), Content: s.text, Importance: s.imp,
		}, emb.Embed(s.text), 24*time.Hour)
		if err != nil {
			return err
		}
		fmt.Printf("  remembered %s  %s\n", id[:8], s.text)
	}

	// Four encounters with the SAME incident. Consolidation needs three close episodes
	// before it fires, and it fires at the END of a cycle — so the lesson cannot influence
	// the run that created it. Encounter 4 is where the agent first acts on its own
	// experience. Anything fewer and the "learning" claim would be asserted, not shown.
	sig := agent.Signal{Kind: "disk_pressure", Detail: "wal segments piling up on roach3", Node: "roach3"}
	labels := []string{
		"2. encounter #1 — no experience, falls back to the innate playbook",
		"3. encounter #2 — still no consolidated lesson",
		"4. encounter #3 — three close episodes now exist: agent consolidates a lesson",
		"5. encounter #4 — THE PAYOFF: decision now comes FROM MEMORY",
	}
	for i, label := range labels {
		fmt.Printf("\n=== %s ===\n", label)
		act, err := a.Handle(ctx, fmt.Sprintf("task-%03d", i+1), sig)
		if err != nil {
			return err
		}
		printAction(act)
	}

	fmt.Println("\n=== 6. audit chain ===")
	rep, err := store.Chain().Verify(ctx, store.DB())
	if err != nil {
		return err
	}
	fmt.Println("  " + rep.String())
	return nil
}

func printAction(act agent.Action) {
	src := "playbook"
	if act.FromMemory {
		src = "MEMORY"
	}
	fmt.Printf("  action=%-45s source=%s\n  why: %s\n", act.Name, src, act.Rationale)
	if act.Learned {
		fmt.Println("  *** consolidated a new durable lesson (semantic memory, never decays) ***")
	}
}

func explain(ctx context.Context, store *memory.Store) error {
	emb := agent.HashEmbedder{}
	vec := emb.Embed("disk pressure wal segments")

	plan, err := store.ExplainRecall(ctx, vec, 3)
	if err != nil {
		return err
	}
	fmt.Println(plan)

	ok, err := store.UsesVectorIndex(ctx, vec)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("REGRESSION: planner is NOT using mem_agent_id_embedding_idx")
	}
	fmt.Println("✅ planner uses the C-SPANN vector index (mem_agent_id_embedding_idx)")
	return nil
}

func recall(ctx context.Context, store *memory.Store, q string) error {
	if q == "" {
		q = "disk filling up"
	}
	emb := agent.HashEmbedder{}
	mems, err := store.Recall(ctx, emb.Embed(q), 5)
	if err != nil {
		return err
	}
	fmt.Printf("query: %q\n\n%-10s %-12s %-9s %s\n", q, "DIST", "KIND", "REGION", "CONTENT")
	for _, m := range mems {
		fmt.Printf("%-10.4f %-12s %-9s %s\n", m.Distance, m.Kind, m.Region, m.Content)
	}
	return nil
}

func verify(ctx context.Context, store *memory.Store, db *sql.DB) error {
	rep, err := store.Chain().Verify(ctx, db)
	if err != nil {
		return err
	}
	fmt.Println(rep.String())
	if !rep.OK() {
		return fmt.Errorf("chain verification FAILED")
	}
	fmt.Println("✅ every link's HMAC recomputes and every prev_hash pointer resolves")
	return nil
}

func belief(ctx context.Context, store *memory.Store, arg string) error {
	d := 60 * time.Second
	if arg != "" {
		parsed, err := time.ParseDuration(arg)
		if err != nil {
			return fmt.Errorf("bad duration %q: %w", arg, err)
		}
		d = parsed
	}
	// gc.ttlseconds bounds this window (default 4h). Past that the MVCC history is gone.
	if d > 4*time.Hour {
		return fmt.Errorf("%s exceeds gc.ttlseconds (4h) — history is garbage collected", d)
	}
	mems, err := store.BeliefAt(ctx, d)
	if err != nil {
		return err
	}
	now, err := store.Recall(ctx, agent.HashEmbedder{}.Embed("disk pressure"), 100)
	if err != nil {
		return err
	}
	// Print both counts side by side. Reading the past alone is confusing: rows deleted since
	// then still appear, which looks like a bug until you see the present for comparison.
	fmt.Printf("memories at now-%s: %d\nmemories right now:  %d\n", d, len(mems), len(now))
	fmt.Printf("(window bounded by gc.ttlseconds = 4h; older history is garbage collected)\n\n")
	for _, m := range mems {
		fmt.Printf("  [%-9s] %s\n", m.Kind, m.Content)
	}
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func run(err error) {
	if err != nil {
		die("%v", err)
	}
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
