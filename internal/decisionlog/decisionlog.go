// SPDX-License-Identifier: MIT

// Package decisionlog is the append-only, hash-chained decision trace the
// paper asks for (§5, l.1687: "immutable audit logs, decision traces ...
// every AI-generated classification is permanently tethered to the human
// operator who authorized its deployment"). Each entry carries the SHA-256
// of the previous one, so any edit, deletion or reordering is detectable with
// Verify. It is written only by the MCP server.
package decisionlog

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Entry is one decision.
type Entry struct {
	Seq   int            `json:"seq"`
	Time  time.Time      `json:"time"`
	Event string         `json:"event"`
	Actor string         `json:"actor" jsonschema:"human (confirmed by the user), agent, or server"`
	Data  map[string]any `json:"data,omitempty"`
	Prev  string         `json:"prev"`
	Hash  string         `json:"hash"`
}

// Genesis is the Prev of the first entry.
const Genesis = "0000000000000000000000000000000000000000000000000000000000000000"

func digest(e Entry) (string, error) {
	e.Hash = ""
	b, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Parse reads JSONL log content.
func Parse(content []byte) ([]Entry, error) {
	var out []Entry
	sc := bufio.NewScanner(bytes.NewReader(content))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("entry %d: %w", len(out)+1, err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// Append returns content with a new entry appended, chained to the last one.
func Append(content []byte, now time.Time, event, actor string, data map[string]any) ([]byte, Entry, error) {
	entries, err := Parse(content)
	if err != nil {
		return nil, Entry{}, err
	}
	e := Entry{Seq: len(entries) + 1, Time: now.UTC().Truncate(time.Millisecond), Event: event, Actor: actor, Data: data, Prev: Genesis}
	if n := len(entries); n > 0 {
		e.Prev = entries[n-1].Hash
	}
	if e.Hash, err = digest(e); err != nil {
		return nil, Entry{}, err
	}
	line, err := json.Marshal(e)
	if err != nil {
		return nil, Entry{}, err
	}
	out := append(append([]byte{}, content...), line...)
	return append(out, '\n'), e, nil
}

// Verify checks the chain and returns the number of valid entries, or the
// first broken entry.
func Verify(content []byte) (int, error) {
	entries, err := Parse(content)
	if err != nil {
		return 0, err
	}
	prev := Genesis
	for i, e := range entries {
		if e.Seq != i+1 {
			return i, fmt.Errorf("entry %d: sequence %d out of order", i+1, e.Seq)
		}
		if e.Prev != prev {
			return i, fmt.Errorf("entry %d: chain broken (prev hash mismatch)", i+1)
		}
		h, err := digest(e)
		if err != nil {
			return i, err
		}
		if h != e.Hash {
			return i, fmt.Errorf("entry %d: content was modified (hash mismatch)", i+1)
		}
		prev = e.Hash
	}
	return len(entries), nil
}
