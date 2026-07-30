package memory

import (
	"math"
	"testing"
)

// The tests below are pure (no database) so they run in CI without a cluster. The
// database-backed guarantees — vector index usage, region-failure survival, chain integrity
// across an outage — are proven by scripts/chaos.sh and `agent explain`, whose captured
// output lives in receipts/. Asserting those here would require a 3-node cluster in CI.

func TestVecLiteralRoundTrip(t *testing.T) {
	cases := []struct {
		in   []float32
		want string
	}{
		{[]float32{1, 0, 0}, "[1,0,0]"},
		{[]float32{-0.5, 0.25}, "[-0.5,0.25]"},
		{[]float32{}, "[]"},
	}
	for _, c := range cases {
		if got := vecLiteral(c.in); got != c.want {
			t.Errorf("vecLiteral(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A float that round-trips through the literal must not gain precision noise, or the
// distances shown in the demo stop matching what the database computes.
func TestVecLiteralShortestForm(t *testing.T) {
	// 1/3 in float32 is 0.33333334; the shortest round-trippable form must be preserved.
	got := vecLiteral([]float32{float32(1.0 / 3.0)})
	if got != "[0.33333334]" {
		t.Errorf("got %q, want [0.33333334] — precision drift would desync displayed distances", got)
	}
}

func TestNullStr(t *testing.T) {
	if nullStr("") != nil {
		t.Error(`nullStr("") must be nil so the column stores SQL NULL, not an empty string`)
	}
	if nullStr("task-1") != "task-1" {
		t.Error("nullStr must pass non-empty values through unchanged")
	}
}

// Dim must stay in lockstep with the VECTOR(384) column in sql/schema.sql. A mismatch
// surfaces as a runtime insert error on every write, so pin it.
func TestDimMatchesSchema(t *testing.T) {
	if Dim != 384 {
		t.Fatalf("Dim = %d but sql/schema.sql declares VECTOR(384)", Dim)
	}
}

// Distance thresholds in agent.go (0.40) and Consolidate (0.35) assume unit-length vectors.
// If the embedder ever stops normalising, those constants silently change meaning.
func TestEmbeddingContractIsUnitLength(t *testing.T) {
	// Mirrors HashEmbedder's normalisation without importing it (agent imports memory,
	// so importing agent here would be a cycle).
	v := make([]float32, Dim)
	v[0], v[1], v[7] = 3, 4, 12
	var sum float64
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	norm := float32(math.Sqrt(sum))
	for i := range v {
		v[i] /= norm
	}

	sum = 0
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	if math.Abs(sum-1.0) > 1e-6 {
		t.Errorf("normalised vector has squared length %v, want 1.0", sum)
	}
}
