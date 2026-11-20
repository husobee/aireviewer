// SPDX-License-Identifier: MIT

// Package voice stores the reviewer's personal voice profile and composes it
// with a repository's team default (ADR 0009). The voice shapes how drafted
// review comments and review bodies are worded; it never changes what they
// claim, and it never affects gate checks.
package voice

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Limits keep the profile a style hint, not a second prompt.
const (
	MaxDescription = 1000
	MaxExamples    = 5
	MaxExample     = 500
	MaxTeam        = 1000
)

// Profile is the reviewer's personal voice, stored on their machine.
type Profile struct {
	Description string    `json:"description" jsonschema:"how the reviewer writes, e.g. direct, warm, asks questions, no emojis"`
	Examples    []string  `json:"examples,omitempty" jsonschema:"up to 5 of the reviewer's own review comments"`
	UseTeam     bool      `json:"use_team_default" jsonschema:"combine with the repository's comments.voice (personal wins on conflicts)"`
	Updated     time.Time `json:"updated"`
}

// Validate checks the limits and rejects empty profiles.
func (p Profile) Validate() error {
	if strings.TrimSpace(p.Description) == "" && len(p.Examples) == 0 {
		return errors.New("a voice needs a description or at least one example")
	}
	if len(p.Description) > MaxDescription {
		return fmt.Errorf("description is %d characters; keep it under %d", len(p.Description), MaxDescription)
	}
	if len(p.Examples) > MaxExamples {
		return fmt.Errorf("at most %d examples", MaxExamples)
	}
	for i, e := range p.Examples {
		if strings.TrimSpace(e) == "" || len(e) > MaxExample {
			return fmt.Errorf("example %d must be 1-%d characters", i+1, MaxExample)
		}
	}
	return nil
}

// Store reads and writes the profile in a directory.
type Store struct {
	Dir string
}

func (s Store) path() string { return filepath.Join(s.Dir, "voice.json") }

// Load returns the profile, or nil when none is set.
func (s Store) Load() (*Profile, error) {
	if s.Dir == "" {
		return nil, nil
	}
	b, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p Profile
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("voice profile %s is corrupt: %w", s.path(), err)
	}
	return &p, nil
}

// Save writes the profile atomically with owner-only permissions.
func (s Store) Save(p Profile) error {
	if s.Dir == "" {
		return errors.New("no user configuration directory")
	}
	if err := p.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(p, "", "  ")
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path())
}

// Clear removes the profile.
func (s Store) Clear() error {
	if s.Dir == "" {
		return nil
	}
	err := os.Remove(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Compose builds the guidance given to drafting agents. It returns "" when
// there is no voice at all. team is the repository default (already read
// from the merge base).
func Compose(p *Profile, team string) string {
	team = strings.TrimSpace(team)
	if len(team) > MaxTeam {
		team = team[:MaxTeam]
	}
	useTeam := team != "" && (p == nil || p.UseTeam)
	if p == nil && !useTeam {
		return ""
	}
	var b strings.Builder
	b.WriteString("Write in this reviewer's voice. The voice affects tone, wording and structure only: it never changes what is claimed, " +
		"never softens or drops a real problem, never adds praise that is not earned, and never overrides the review rules or the comment checks.\n")
	if useTeam {
		fmt.Fprintf(&b, "\nTeam style (repository default):\n%s\n", team)
	}
	if p != nil {
		if d := strings.TrimSpace(p.Description); d != "" {
			label := "Personal voice"
			if useTeam {
				label += " (takes precedence over the team style)"
			}
			fmt.Fprintf(&b, "\n%s:\n%s\n", label, d)
		}
		if len(p.Examples) > 0 {
			b.WriteString("\nExamples of this reviewer's own comments (match their style, not their content):\n")
			for _, e := range p.Examples {
				fmt.Fprintf(&b, "- %q\n", strings.TrimSpace(e))
			}
		}
	}
	return b.String()
}

// DefaultDir is $AIREVIEWER_CONFIG_DIR or <user config dir>/aireviewer.
func DefaultDir() string {
	if d := os.Getenv("AIREVIEWER_CONFIG_DIR"); d != "" {
		return d
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "aireviewer")
}
