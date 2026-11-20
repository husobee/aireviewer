// SPDX-License-Identifier: MIT

package mcpserver

import (
	"context"
	"errors"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/husobee/aireviewer/internal/voice"
)

// M5.1: the reviewer's voice (ADR 0009).

func registerVoiceTools(s *mcp.Server, h *handlers) {
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	write := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true}
	mcp.AddTool(s, &mcp.Tool{Name: "voice_get", Annotations: ro,
		Description: "Return the reviewer's personal voice profile, the repository's team default (comments.voice from the merge-base " +
			".aireviewer.yml, when run_id is given), and the composed guidance to give drafting agents. Empty guidance means no voice is set."},
		h.voiceGet)
	mcp.AddTool(s, &mcp.Tool{Name: "voice_set", Annotations: write,
		Description: "Save the reviewer's personal voice profile on this machine (their configuration directory): a description of how " +
			"they write and up to 5 of their own example comments. Only when the reviewer explicitly asks; show them what will be saved. " +
			"The voice shapes wording only and never overrides review rules or comment checks."},
		h.voiceSet)
	mcp.AddTool(s, &mcp.Tool{Name: "voice_clear", Annotations: write,
		Description: "Delete the reviewer's personal voice profile. Only when the reviewer explicitly asks."},
		h.voiceClear)
}

type VoiceGetIn struct {
	RunID string `json:"run_id,omitempty" jsonschema:"include the repository's team default for this run"`
}

type VoiceOut struct {
	Personal *voice.Profile `json:"personal,omitempty"`
	Team     string         `json:"team,omitempty" jsonschema:"comments.voice from the merge-base .aireviewer.yml"`
	Guidance string         `json:"guidance" jsonschema:"pass verbatim as 'voice' to drafting agents; empty when no voice is set"`
	Path     string         `json:"path,omitempty"`
}

func (h *handlers) voiceStore() voice.Store { return voice.Store{Dir: h.cfg.VoiceDir} }

func (h *handlers) voiceGet(ctx context.Context, _ *mcp.CallToolRequest, in VoiceGetIn) (*mcp.CallToolResult, VoiceOut, error) {
	p, err := h.voiceStore().Load()
	if err != nil {
		return nil, VoiceOut{}, err
	}
	out := VoiceOut{Personal: p}
	if h.cfg.VoiceDir != "" {
		out.Path = h.cfg.VoiceDir + "/voice.json"
	}
	if in.RunID != "" {
		st, err := h.loadCheckout(in.RunID)
		if err != nil {
			return nil, VoiceOut{}, err
		}
		cfg, _ := h.loadConfig(ctx, st)
		out.Team = strings.TrimSpace(cfg.Comments.Voice)
	}
	out.Guidance = voice.Compose(p, out.Team)
	return nil, out, nil
}

type VoiceSetIn struct {
	Description    string   `json:"description" jsonschema:"how the reviewer writes, in their own words"`
	Examples       []string `json:"examples,omitempty" jsonschema:"up to 5 of the reviewer's own review comments"`
	UseTeamDefault *bool    `json:"use_team_default,omitempty" jsonschema:"combine with a repository's comments.voice (default true)"`
}

func (h *handlers) voiceSet(_ context.Context, _ *mcp.CallToolRequest, in VoiceSetIn) (*mcp.CallToolResult, VoiceOut, error) {
	if h.cfg.VoiceDir == "" {
		return nil, VoiceOut{}, errors.New("no user configuration directory is available for a personal voice")
	}
	p := voice.Profile{Description: strings.TrimSpace(in.Description), Examples: in.Examples, UseTeam: true, Updated: h.now()}
	if in.UseTeamDefault != nil {
		p.UseTeam = *in.UseTeamDefault
	}
	if err := h.voiceStore().Save(p); err != nil {
		return nil, VoiceOut{}, err
	}
	return nil, VoiceOut{Personal: &p, Guidance: voice.Compose(&p, ""), Path: h.cfg.VoiceDir + "/voice.json"}, nil
}

func (h *handlers) voiceClear(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, OKOut, error) {
	if err := h.voiceStore().Clear(); err != nil {
		return nil, OKOut{}, err
	}
	return nil, OKOut{OK: true}, nil
}
