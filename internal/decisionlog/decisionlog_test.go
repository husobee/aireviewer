// SPDX-License-Identifier: MIT

package decisionlog

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestChain(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	var log []byte
	var err error
	for i, ev := range []string{"proposal_created", "human_confirmed", "posted"} {
		log, _, err = Append(log, now.Add(time.Duration(i)*time.Second), ev, "server", map[string]any{"n": i})
		if err != nil {
			t.Fatal(err)
		}
	}
	if n, err := Verify(log); err != nil || n != 3 {
		t.Fatalf("verify = %d, %v", n, err)
	}
	// Tamper with content.
	bad := bytes.Replace(log, []byte(`"posted"`), []byte(`"declined"`), 1)
	if _, err := Verify(bad); err == nil || !strings.Contains(err.Error(), "entry 3") {
		t.Errorf("tamper not detected: %v", err)
	}
	// Delete the middle entry.
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	cut := []byte(lines[0] + "\n" + lines[2] + "\n")
	if _, err := Verify(cut); err == nil {
		t.Error("deletion not detected")
	}
	// Drop the last entry: still a valid (shorter) chain — truncation is only
	// detectable against an externally recorded head hash.
	if n, err := Verify([]byte(lines[0] + "\n" + lines[1] + "\n")); err != nil || n != 2 {
		t.Errorf("truncated = %d, %v", n, err)
	}
}
