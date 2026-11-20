// SPDX-License-Identifier: MIT

package voice

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	bad := []Profile{
		{},
		{Description: strings.Repeat("x", MaxDescription+1)},
		{Description: "d", Examples: []string{"1", "2", "3", "4", "5", "6"}},
		{Description: "d", Examples: []string{" "}},
		{Description: "d", Examples: []string{strings.Repeat("x", MaxExample+1)}},
	}
	for i, p := range bad {
		if p.Validate() == nil {
			t.Errorf("case %d accepted: %+v", i, p)
		}
	}
	if err := (Profile{Examples: []string{"Nit: could this be a const?"}}).Validate(); err != nil {
		t.Errorf("examples-only profile rejected: %v", err)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "cfg")}
	if p, err := s.Load(); p != nil || err != nil {
		t.Fatalf("empty load = %+v, %v", p, err)
	}
	if err := s.Save(Profile{Description: "Direct and warm.", UseTeam: true}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(s.Dir, "voice.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("profile mode = %v, %v", fi.Mode(), err)
	}
	p, err := s.Load()
	if err != nil || p.Description != "Direct and warm." || !p.UseTeam {
		t.Errorf("load = %+v, %v", p, err)
	}
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Load(); p != nil {
		t.Error("profile survived Clear")
	}
	if err := (Store{}).Save(Profile{Description: "x"}); err == nil {
		t.Error("saved without a directory")
	}
}

func TestCompose(t *testing.T) {
	if Compose(nil, "") != "" {
		t.Error("no voice should compose to empty guidance")
	}
	team := Compose(nil, "Be concise; prefer questions.")
	if !strings.Contains(team, "Team style") || strings.Contains(team, "Personal") {
		t.Errorf("team only:\n%s", team)
	}
	p := &Profile{Description: "Direct, warm, no emojis.", Examples: []string{"Could we guard against nil here?"}, UseTeam: true}
	both := Compose(p, "Be concise.")
	for _, want := range []string{"never softens or drops a real problem", "Team style", "takes precedence", "Direct, warm, no emojis.", `"Could we guard against nil here?"`} {
		if !strings.Contains(both, want) {
			t.Errorf("composed lacks %q:\n%s", want, both)
		}
	}
	if strings.Index(both, "Team style") > strings.Index(both, "Personal voice") {
		t.Error("personal voice must come after (and override) the team style")
	}
	p.UseTeam = false
	if mine := Compose(p, "Be concise."); strings.Contains(mine, "Team style") {
		t.Errorf("opted-out team default still included:\n%s", mine)
	}
}
