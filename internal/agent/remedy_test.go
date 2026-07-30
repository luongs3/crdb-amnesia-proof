package agent

import "testing"

// extractRemedy parses consolidated runbook lines back into an action. Remedies legitimately
// contain commas, and an earlier LastIndex-based split silently truncated
// "drain the lagging node, then restart it" to "then restart it" — the agent appeared to
// recall a lesson but acted on a fragment of it. Caught by watching the dashboard, not by
// any test, so these lock the behaviour down.
func TestExtractRemedy(t *testing.T) {
	cases := []struct{ lesson, want string }{
		{"runbook: for disk_pressure, truncate WAL and expand the volume before paging",
			"truncate WAL and expand the volume before paging"},
		{"runbook: for replication_lag, drain the lagging node, then restart it",
			"drain the lagging node, then restart it"},
		{"runbook: for node_down, verify quorum, then rejoin the node",
			"verify quorum, then rejoin the node"},
		{"nonsense with no separator", "escalate"},
	}
	for _, c := range cases {
		if got := extractRemedy(c.lesson); got != c.want {
			t.Errorf("extractRemedy(%q)\n got: %q\nwant: %q", c.lesson, got, c.want)
		}
	}
}

// Every remedy the agent can consolidate must survive the round trip through a lesson string.
func TestRemedyRoundTrip(t *testing.T) {
	for _, kind := range []string{"disk_pressure", "replication_lag", "node_down"} {
		remedy := preferredRemedy(kind)
		lesson := "runbook: for " + kind + ", " + remedy
		if got := extractRemedy(lesson); got != remedy {
			t.Errorf("round trip failed for %s\n got: %q\nwant: %q", kind, got, remedy)
		}
	}
}
