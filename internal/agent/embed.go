package agent

import (
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"unicode"

	"github.com/luongs3/crdb-amnesia-proof/internal/memory"
)

// HashEmbedder is a deterministic local embedder: character-trigram + word hashing with
// sublinear term weighting, L2-normalised.
//
// WHY NOT A HOSTED MODEL: the AWS requirement is met by Lambda + S3, and Bedrock model access
// can sit hours-to-days in approval. Putting the core loop behind someone else's approval
// queue is how a submission misses a deadline. This runs offline and is deterministic, so
// chaos.sh produces identical distances on every run and the receipts stay auditable.
//
// WHY TRIGRAMS AND NOT JUST WORDS: an earlier version hashed whole words only. Every query
// that shared no exact tokens with a memory landed at the same distance, and any query that
// matched exactly landed at 0.0000. Both are tells that a demo is retrieving strings rather
// than measuring similarity. Character trigrams give partial credit for "wal"/"WAL segments"/
// "write-ahead log", so distances spread out and rephrasing still recalls — which is the
// property the whole "semantic memory" claim depends on.
//
// This is lexical similarity, not a neural embedding, and the README says so.
type HashEmbedder struct{}

func (HashEmbedder) Embed(text string) []float32 {
	v := make([]float32, memory.Dim)

	add := func(feature string, w float32) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(feature))
		idx := int(h.Sum32() % uint32(memory.Dim))
		// A second hash picks the sign, so unrelated features colliding in one bucket tend
		// to cancel instead of compounding into false similarity.
		sh := fnv.New32()
		_, _ = sh.Write([]byte("sign:" + feature))
		if sh.Sum32()%2 == 0 {
			v[idx] += w
		} else {
			v[idx] -= w
		}
	}

	toks := tokenize(text)
	for i, t := range toks {
		add("w:"+t, 1.0)
		if i+1 < len(toks) {
			add("b:"+t+"_"+toks[i+1], 1.4) // bigrams carry more signal than bare unigrams
		}
		// Character trigrams over each token: this is what lets "wal" partially match
		// "walsegments" and "truncating" match "truncate".
		padded := "^" + t + "$"
		for j := 0; j+3 <= len(padded); j++ {
			add("t:"+padded[j:j+3], 0.55)
		}
	}

	// Densify. With 384 dimensions and a handful of sparse features per document, two
	// unrelated unit vectors share almost no non-zero buckets, so their L2 distance pins to
	// sqrt(2) ~= 1.4142 and EVERY unrelated pair reports the same number. That looks broken
	// (and staged) in a demo even though the ranking underneath is correct.
	//
	// Spreading each feature across several buckets with decaying weight gives documents
	// overlapping support, so distances vary continuously with actual similarity. This is
	// the standard "multiple hashes per feature" trick from feature hashing.
	for i, t := range toks {
		for k := 1; k <= 3; k++ {
			add(fmt.Sprintf("w%d:%s", k, t), 0.45/float32(k))
		}
		if i+1 < len(toks) {
			add("b2:"+t+"_"+toks[i+1], 0.5)
		}
	}

	var sum float64
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	if sum == 0 {
		v[0] = 1 // never emit a zero vector; the index cannot meaningfully order it
		return v
	}
	// L2-normalise: the distance thresholds in agent.go (0.40) and Consolidate (0.35)
	// assume unit vectors.
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
	"not": true, "its": true,
}
