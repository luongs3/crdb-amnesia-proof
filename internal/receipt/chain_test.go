package receipt

import (
	"testing"
)

func TestLinkIsDeterministic(t *testing.T) {
	c := NewChain("agent_a1", []byte("k"))
	a := c.link(1, "remember:episodic", "mem-1", Genesis)
	b := c.link(1, "remember:episodic", "mem-1", Genesis)
	if a != b {
		t.Fatal("same inputs must produce the same HMAC, or Verify can never pass")
	}
}

// Every field that goes into the HMAC must actually change it. If any of these were dropped
// from the signed payload, that field could be tampered with after the fact undetected.
func TestEveryFieldIsSigned(t *testing.T) {
	c := NewChain("agent_a1", []byte("k"))
	base := c.link(1, "act:restart", "mem-1", Genesis)

	variants := map[string]string{
		"seq":      c.link(2, "act:restart", "mem-1", Genesis),
		"event":    c.link(1, "act:drain", "mem-1", Genesis),
		"memoryID": c.link(1, "act:restart", "mem-2", Genesis),
		"prevHash": c.link(1, "act:restart", "mem-1", "other"),
	}
	for field, h := range variants {
		if h == base {
			t.Errorf("changing %s did not change the hash — field is not covered by the HMAC", field)
		}
	}
}

// A different agent with the same event history must not produce the same chain, otherwise
// one agent's receipts could be replayed as another's.
func TestAgentIDIsSigned(t *testing.T) {
	a := NewChain("agent_a1", []byte("k")).link(1, "e", "m", Genesis)
	b := NewChain("agent_a2", []byte("k")).link(1, "e", "m", Genesis)
	if a == b {
		t.Error("agent_id must be part of the signed payload")
	}
}

func TestKeyMatters(t *testing.T) {
	a := NewChain("agent_a1", []byte("key-1")).link(1, "e", "m", Genesis)
	b := NewChain("agent_a1", []byte("key-2")).link(1, "e", "m", Genesis)
	if a == b {
		t.Error("a different HMAC key must produce a different hash")
	}
}

func TestVerifyReportOK(t *testing.T) {
	if !(VerifyReport{Links: 3}).OK() {
		t.Error("a clean report must be OK")
	}
	for name, r := range map[string]VerifyReport{
		"gap":         {Links: 3, Gaps: []int64{2}},
		"bad hash":    {Links: 3, BadHashes: []int64{2}},
		"broken link": {Links: 3, BrokenLinks: []int64{2}},
	} {
		if r.OK() {
			t.Errorf("report with a %s must not be OK", name)
		}
	}
}
