// Package memory is the agent's memory layer, backed entirely by CockroachDB.
//
// Design constraint that shapes this whole file: the vector index on `memories` has
// (crdb_region, agent_id, embedding) as its prefix, because REGIONAL BY ROW injects
// crdb_region as an implicit leading column. Verified live on v26.2.4:
//
//	WHERE agent_id = $1                          -> • vector search (index used)
//	WHERE agent_id = $1 AND crdb_region = $2     -> • scan          (index NOT used)
//
// Pinning the region collapses the query to a single-region span and the planner prefers a
// plain scan of it. So Recall deliberately constrains ONLY agent_id and lets the vector
// search fan out across all three regional prefix spans. Do not "optimize" by adding a
// region predicate — it silently costs you the index.
//
// Second verified trap: a subquery-built vector defeats the index where an identical
// literal does not. Embeddings are always passed as bound parameters.
package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/luongs3/crdb-amnesia-proof/internal/receipt"
)

// Dim is the embedding width. 384 matches all-MiniLM-L6-v2, the local model used so the
// demo never depends on an external embedding API being approved.
const Dim = 384

type Kind string

const (
	Episodic   Kind = "episodic"   // raw experience, decays
	Semantic   Kind = "semantic"   // consolidated knowledge, never decays
	Checkpoint Kind = "checkpoint" // task progress, decays fast
)

type Memory struct {
	ID         string
	AgentID    string
	Kind       Kind
	Content    string
	Importance float64
	TaskID     string
	Region     string
	CreatedAt  time.Time
	Distance   float64 // populated by Recall only
}

type Store struct {
	db      *sql.DB
	agentID string
	chain   *receipt.Chain
}

func NewStore(db *sql.DB, agentID string, hmacKey []byte) *Store {
	return &Store{db: db, agentID: agentID, chain: receipt.NewChain(agentID, hmacKey)}
}

func (s *Store) Chain() *receipt.Chain { return s.chain }

// DB exposes the underlying handle so callers can verify the audit chain without threading a
// second *sql.DB alongside every Store.
func (s *Store) DB() *sql.DB { return s.db }

// vecLiteral renders a float32 slice as a CockroachDB VECTOR literal.
// strconv with -1 precision keeps the shortest round-trippable form, which keeps the
// visible distances in the demo honest (no rounding artifacts).
func vecLiteral(v []float32) string {
	var b strings.Builder
	b.Grow(len(v) * 8)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// Remember writes a memory and its audit receipt in ONE serializable transaction.
//
// This is the claim the sponsor's own copy makes ("no consistency gaps between your vector
// data and your operational database") and the reason a bolt-on vector store cannot compete:
// there is no window in which the embedding exists without its audit link, or vice versa.
//
// Region: REGIONAL BY ROW defaults crdb_region to the GATEWAY's region, so every row written
// through one node lands in that node's region and the multi-region story is invisible. Real
// agents are geo-distributed, so callers may pin a region explicitly; when they do, the row
// is homed there and reads from that region are local.
func (s *Store) Remember(ctx context.Context, m Memory, embedding []float32, ttl time.Duration) (string, error) {
	if len(embedding) != Dim {
		return "", fmt.Errorf("embedding must be %d dims, got %d", Dim, len(embedding))
	}
	if m.Kind == "" {
		m.Kind = Episodic
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	// Row-Level TTL reads expires_at via ttl_expiration_expression. NULL = never forget,
	// which is how consolidated Semantic memories outlive the episodes they came from.
	var expires any
	if ttl > 0 {
		expires = time.Now().Add(ttl)
	}

	var id, region string
	if m.Region != "" {
		err = tx.QueryRowContext(ctx, `
			INSERT INTO memories (agent_id, kind, content, embedding, importance, expires_at, task_id, crdb_region)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING id::STRING, crdb_region::STRING`,
			s.agentID, string(m.Kind), m.Content, vecLiteral(embedding), m.Importance,
			expires, nullStr(m.TaskID), m.Region,
		).Scan(&id, &region)
	} else {
		err = tx.QueryRowContext(ctx, `
			INSERT INTO memories (agent_id, kind, content, embedding, importance, expires_at, task_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING id::STRING, crdb_region::STRING`,
			s.agentID, string(m.Kind), m.Content, vecLiteral(embedding), m.Importance,
			expires, nullStr(m.TaskID),
		).Scan(&id, &region)
	}
	if err != nil {
		return "", fmt.Errorf("insert memory: %w", err)
	}

	if _, _, err := s.chain.Append(ctx, tx, "remember:"+string(m.Kind), id); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit memory+receipt: %w", err)
	}
	return id, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Recall does approximate nearest-neighbour retrieval over this agent's memories.
//
// NOTE the predicate: agent_id ONLY. See the package comment — adding crdb_region here
// would silently drop the vector index.
func (s *Store) Recall(ctx context.Context, embedding []float32, k int) ([]Memory, error) {
	if len(embedding) != Dim {
		return nil, fmt.Errorf("embedding must be %d dims, got %d", Dim, len(embedding))
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id::STRING, kind, content, importance, crdb_region::STRING, created_at,
		       embedding <-> $2 AS dist
		  FROM memories
		 WHERE agent_id = $1
		 ORDER BY dist
		 LIMIT $3`,
		s.agentID, vecLiteral(embedding), k)
	if err != nil {
		return nil, fmt.Errorf("recall: %w", err)
	}
	defer rows.Close()

	var out []Memory
	for rows.Next() {
		var m Memory
		var kind string
		if err := rows.Scan(&m.ID, &kind, &m.Content, &m.Importance, &m.Region, &m.CreatedAt, &m.Distance); err != nil {
			return nil, err
		}
		m.Kind = Kind(kind)
		m.AgentID = s.agentID
		out = append(out, m)
	}
	return out, rows.Err()
}

// ExplainRecall returns the query plan for a Recall.
//
// Shipped as a first-class method, not a test helper, because "the vector index is actually
// used" is the single claim most submissions will assert without evidence. The demo prints
// this on camera.
func (s *Store) ExplainRecall(ctx context.Context, embedding []float32, k int) (string, error) {
	rows, err := s.db.QueryContext(ctx, `
		EXPLAIN SELECT content FROM memories WHERE agent_id = $1 ORDER BY embedding <-> $2 LIMIT $3`,
		s.agentID, vecLiteral(embedding), k)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", err
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String(), rows.Err()
}

// UsesVectorIndex reports whether the planner chose the C-SPANN index.
// Used by the smoke test to fail loudly if a schema change silently regresses the plan.
func (s *Store) UsesVectorIndex(ctx context.Context, embedding []float32) (bool, error) {
	plan, err := s.ExplainRecall(ctx, embedding, 3)
	if err != nil {
		return false, err
	}
	return strings.Contains(plan, "vector search") &&
		strings.Contains(plan, "mem_agent_id_embedding_idx"), nil
}

// BeliefAt answers "what did the agent know at time T?" using AS OF SYSTEM TIME.
//
// This is decision replay, and no standalone vector store can do it. Two constraints learned
// live: (1) the window is bounded by gc.ttlseconds, default 4h — not unlimited time travel;
// (2) AS OF SYSTEM TIME cannot be interpolated as a bind parameter, so the caller-supplied
// duration is formatted into the statement. Only accept an internally-generated duration here.
func (s *Store) BeliefAt(ctx context.Context, ago time.Duration) ([]Memory, error) {
	stmt := fmt.Sprintf(`
		SELECT id::STRING, kind, content, importance, crdb_region::STRING, created_at
		  FROM memories AS OF SYSTEM TIME '-%ds'
		 WHERE agent_id = $1 ORDER BY created_at`, int64(ago.Seconds()))

	rows, err := s.db.QueryContext(ctx, stmt, s.agentID)
	if err != nil {
		return nil, fmt.Errorf("belief replay: %w", err)
	}
	defer rows.Close()

	var out []Memory
	for rows.Next() {
		var m Memory
		var kind string
		if err := rows.Scan(&m.ID, &kind, &m.Content, &m.Importance, &m.Region, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.Kind = Kind(kind)
		m.AgentID = s.agentID
		out = append(out, m)
	}
	return out, rows.Err()
}

// RecallLesson finds the closest CONSOLIDATED (semantic) memory, if any is close enough.
//
// Implemented as an over-fetch plus an in-Go filter rather than `AND kind = 'semantic'`,
// because a kind predicate defeats the vector index exactly like a region predicate does
// (verified live: the plan degrades to `• filter -> • scan table: memories@pk_memories`).
// So we keep the indexed ANN query and narrow afterwards.
//
// This also fixes a real regression: as episodic memories accumulate, they crowd a single
// consolidated lesson out of a small top-k, and the agent silently stops using what it
// learned. Over-fetching keeps the lesson reachable no matter how much raw experience piles up.
// preferRegion, when supplied, biases selection toward a lesson homed in that region. The
// demo uses it to surface a recall served FROM a region that is currently down — which is
// the difference between asserting survivability ("27/27 readable" would print the same if
// nothing were read) and demonstrating it.
func (s *Store) RecallLesson(ctx context.Context, embedding []float32, maxDist float64, preferRegion ...string) (*Memory, error) {
	const candidates = 50
	mems, err := s.Recall(ctx, embedding, candidates)
	if err != nil {
		return nil, err
	}
	want := ""
	if len(preferRegion) > 0 {
		want = preferRegion[0]
	}
	var first *Memory
	for i := range mems {
		m := mems[i]
		if m.Kind != Semantic || m.Distance > maxDist {
			continue
		}
		if first == nil {
			first = &m
		}
		if want != "" && m.Region == want {
			return &m, nil
		}
	}
	return first, nil
}

// Consolidate promotes repeated episodic experience into one durable semantic memory.
//
// This is the "memory consolidation" mechanism: after seeing the same class of incident
// enough times, the agent stops re-deriving the lesson and writes it down permanently
// (expires_at = NULL). The episodes still decay via TTL; the lesson does not.
//
// Idempotent: if a semantic memory for this incident class already exists, this is a no-op.
// Without that check the agent re-learns the same lesson on every subsequent encounter and
// the semantic tier fills with duplicates.
func (s *Store) Consolidate(ctx context.Context, embedding []float32, lesson string, minEpisodes int, maxDist float64, region ...string) (string, bool, error) {
	similar, err := s.Recall(ctx, embedding, minEpisodes+4)
	if err != nil {
		return "", false, err
	}
	var n int
	for _, m := range similar {
		switch {
		case m.Kind == Semantic && m.Content == lesson:
			return m.ID, false, nil // already learned
		case m.Kind == Episodic && m.Distance <= maxDist:
			// Count only episodic neighbours describing the same incident class. The
			// caller supplies the threshold because it is measured against the embedder
			// in use, not a universal constant.
			n++
		}
	}
	if n < minEpisodes {
		return "", false, nil
	}

	// Home the lesson where the incidents were observed. Left to default, every lesson
	// lands in the gateway's region, so a lesson can never be recalled FROM a downed
	// region — which is exactly the demonstration this project exists to make.
	home := ""
	if len(region) > 0 {
		home = region[0]
	}
	id, err := s.Remember(ctx, Memory{
		Kind:       Semantic,
		Content:    lesson,
		Importance: 1.0,
		Region:     home,
	}, embedding, 0) // ttl 0 -> never forget
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}
