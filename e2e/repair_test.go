package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// repairReport is the answer to POST /repair.
type repairReport struct {
	From      string `json:"from"`
	Rewritten int    `json:"rewritten"`
	Absent    int    `json:"absent"`
	Refused   int    `json:"refused"`
	Keys      []struct {
		Key     string `json:"key"`
		Status  string `json:"status"`
		Version string `json:"version"`
		Error   string `json:"error"`
	} `json:"keys"`
}

// repair makes this node's copy of the named keys authoritative.
func (n node) repair(t *testing.T, keys ...string) repairReport {
	t.Helper()
	body, err := json.Marshal(map[string][]string{"keys": keys})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	code, _, out := n.do(t, http.MethodPost, "/repair", string(body), nil)
	if code != http.StatusOK {
		t.Fatalf("%s: POST /repair = %d: %s", n.name, code, out)
	}
	var report repairReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode repair report: %v", err)
	}
	return report
}

// The cure for the condition the twins are in. They hold different values for
// one key at the identical version, so anti-entropy compares versions, finds
// them equal and is right to do nothing — this cluster stays split for as long
// as both nodes run, and the consistency check can only keep saying so.
//
// This runs after TestTwinsUnderOneIdentityAreReportedNotHidden, which asserts
// the split exists. Order matters: tests here run in the order they are
// written, and this one ends the split deliberately.
func TestRepairEndsASplitAntiEntropyCannot(t *testing.T) {
	const key = "split-brain"

	before, _, ok := twinA.get(t, key)
	if !ok {
		t.Fatalf("twin-a does not hold %s", key)
	}
	if other, _, ok := twinB.get(t, key); !ok || other == before {
		t.Fatalf("the twins are not split on %s: a=%q b=%q (%v)", key, before, other, ok)
	}

	report := twinA.repair(t, key)
	if report.Rewritten != 1 {
		t.Fatalf("repaired %d key(s), want 1: %+v", report.Rewritten, report.Keys)
	}
	if report.From == "" {
		t.Error("the report does not name the node whose copy was made authoritative")
	}

	// twin-b takes it, because the rewrite carries a version that is genuinely
	// newer rather than merely different.
	eventually(t, "twin-b to take twin-a's value", converge, func() (bool, string) {
		got, _, ok := twinB.get(t, key)
		if !ok {
			return false, "twin-b no longer holds the key"
		}
		if got != before {
			return false, "twin-b still holds " + got
		}
		return true, ""
	})

	// And the check that reported the split now reports agreement, which is
	// the only assertion that matters.
	eventually(t, "the twins to agree", converge, func() (bool, string) {
		r := twinA.consistency(t)
		if !r.Converged {
			return false, "the report still says the replicas disagree"
		}
		return true, ""
	})
}

// A key this node does not hold cannot be made authoritative from here, and
// deleting it everywhere instead would be a destructive guess about which
// replica was right.
func TestRepairReportsAKeyTheNodeDoesNotHold(t *testing.T) {
	report := a.repair(t, "never-written-anywhere")
	if report.Absent != 1 || report.Rewritten != 0 {
		t.Fatalf("report = %+v, want one absent and nothing rewritten", report)
	}
}

// "Repair everything" on the wrong node overwrites a cluster with stale data.
func TestRepairRefusesToGuessWhichKeys(t *testing.T) {
	code, _, _ := a.do(t, http.MethodPost, "/repair", `{"keys":[]}`, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

// Found by reading the code for this round: a value of arbitrary bytes was
// stored intact here and mangled on every replica — at the same version, so
// anti-entropy saw agreement and nothing could ever repair it. Refusing is the
// only honest answer while bodies are JSON.
func TestABinaryValueIsRefusedAcrossTheCluster(t *testing.T) {
	code, _, _ := a.do(t, http.MethodPut, "/kv/binary-value", "\x00\xff\xfe\x80A", nil)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", code)
	}
	// Refused everywhere, not merely here: no replica may end up holding it.
	for _, n := range core {
		if _, _, ok := n.get(t, "binary-value"); ok {
			t.Errorf("%s holds a value that cannot be replicated intact", n.name)
		}
	}
}

// The refusal is about bytes that cannot survive, not bytes that look unusual.
// Everything valid still has to cross the cluster byte for byte.
func TestAwkwardButValidValuesStillReplicate(t *testing.T) {
	values := map[string]string{
		"utf8/emoji":    "🙂 ünïcödé 日本語",
		"utf8/control":  "\x00\x01\x02",
		"utf8/quotes":   `"quoted" \backslashed\`,
		"utf8/newlines": "one\ntwo\r\nthree",
	}
	for key, value := range values {
		a.put(t, key, value)
	}
	for key, value := range values {
		for _, n := range core {
			eventually(t, n.name+" to hold "+key, converge, func() (bool, string) {
				got, _, ok := n.get(t, key)
				if !ok {
					return false, "not there yet"
				}
				if got != value {
					return false, "holds different bytes: " + strings.ToValidUTF8(got, "?")
				}
				return true, ""
			})
		}
	}
}

// The rendezvous service is the only component that sees where a node's
// packets really come from, and it used that address only to pick a reply path.
func TestTheRosterReportsWhereANodeWasSeen(t *testing.T) {
	// Every node here advertises its container name, which is what the others
	// reach it by — so there is nothing to report as wrong, and the check must
	// stay silent rather than inventing a mismatch.
	for _, c := range a.diagnostics(t).Checks {
		if strings.Contains(c.Title, "does not see this node at") {
			t.Errorf("a correctly-configured node was told its address is wrong: %s", c.Detail)
		}
	}
	// And the snapshot carries the observation itself once bootstrap answers.
	eventually(t, "bootstrap to report where this node was seen", settle, func() (bool, string) {
		var payload struct {
			Snapshot struct {
				SeenAt string `json:"seen_at"`
			} `json:"snapshot"`
		}
		a.getJSON(t, "/diagnostics", &payload)
		if payload.Snapshot.SeenAt == "" {
			return false, "no observation recorded yet"
		}
		return true, ""
	})
}
