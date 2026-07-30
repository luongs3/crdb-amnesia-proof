package agent

import (
	"hash/fnv"
	"math"
	"strings"
	"unicode"

	"github.com/luongs3/crdb-amnesia-proof/internal/memory"
)

// HashEmbedder is a deterministic local embedder: hashed bag-of-words (the "hashing trick")
// over unigrams and bigrams, L2-normalised.
//
// WHY NOT A REAL MODEL: the AWS requirement is satisfied by Lambda + S3, and Bedrock model
// access can sit hours-to-days in approval. Making the core loop depend on an external
// embedding API would put the whole submission behind someone else's approval queue. This
// runs offline, is deterministic (so `chaos.sh` produces identical distances on every run,
// which is what makes the receipts auditable), and is honest about what it is — semantic
// similarity here comes from lexical overlap, which is sufficient to demonstrate a memory
// substrate. The Lambda path optionally re-embeds with a real model; nothing depends on it.
type HashEmbedder struct{}

func (HashEmbedder) Embed(text string) []float32 {
	v := make([]float32, memory.Dim)
	toks := tokenize(text)

	add := func(s string, w float32) {
		h := fnv.New32a()
		h.Write([]byte(s))
		idx := int(h.Sum32() % uint32(memory.Dim))
		// Signed buckets: a second hash decides polarity so distinct tokens colliding in the
		// same bucket tend to cancel rather than compound.
		sh := fnv.New32()
		sh.Write([]byte("sign:" + s))
		if sh.Sum32()%2 == 0 {
			v[idx] += w
		} else {
			v[idx] -= w
		}
	}

	for i, t := range toks {
		add(t, 1)
		if i+1 < len(toks) {
			add(t+"_"+toks[i+1], 1.5) // bigrams carry more signal than bare unigrams
		}
	}

	// L2-normalise so the <-> (L2) operator behaves like a similarity measure with a stable
	// scale. Distance thresholds in agent.go and Consolidate assume unit vectors.
	var sum float64
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	if sum == 0 {
		v[0] = 1 // never emit a zero vector; the index cannot order it meaningfully
		return v
	}
	norm := float32(math.Sqrt(sum))
	for i := range v {
		v[i] /= norm
	}
	return v
}

func tokenize(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := fields[:0]
	for _, f := range fields {
		if len(f) > 1 && !stopwords[f] {
			out = append(out, f)
		}
	}
	return out
}

var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true,
	"this": true, "was": true, "are": true, "has": true, "have": true,
}
