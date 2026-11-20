// SPDX-License-Identifier: MIT

package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/husobee/aireviewer/internal/forge"
	"github.com/husobee/aireviewer/internal/gitrepo"
	"github.com/husobee/aireviewer/internal/runctx"
)

func TestVoiceTools(t *testing.T) {
	// The merge base sets a team voice (read like all config from the merge
	// base; see TestRuntimeFlow for a PR that tries to change config).
	src, head := runtimeRepo(t, "comments:\n  voice: \"Be concise and kind; prefer questions over commands.\"\n")
	root := t.TempDir()
	srv := New(Config{Version: "test", RepoRoot: root, Store: runctx.New(root), CacheDir: t.TempDir(), VoiceDir: t.TempDir(),
		Providers: gateSource{&gateProvider{prProvider: prProvider{head: head}, author: "alice", user: "bob"}},
		RemoteFor: func(repo forge.Repo, tok string) gitrepo.Remote {
			return gitrepo.Remote{URL: "file://" + src, Kind: repo.Forge, Token: tok}
		}})
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	run := call[CheckoutOut](t, cs, "pr_checkout", map[string]any{"url": "https://github.com/o/r/pull/3"}).RunID

	if v := call[VoiceOut](t, cs, "voice_get", map[string]any{}); v.Guidance != "" || v.Personal != nil {
		t.Errorf("no voice yet: %+v", v)
	}
	v := call[VoiceOut](t, cs, "voice_get", map[string]any{"run_id": run})
	if !strings.Contains(v.Team, "prefer questions") || !strings.Contains(v.Guidance, "Team style") {
		t.Errorf("team default = %+v", v)
	}

	if msg := callErr(t, cs, "voice_set", map[string]any{"description": ""}); !strings.Contains(msg, "description or at least one example") {
		t.Errorf("empty voice: %s", msg)
	}
	call[VoiceOut](t, cs, "voice_set", map[string]any{"description": "Direct and warm. No emojis.",
		"examples": []string{"This loop never sleeps between retries; could we add a backoff?"}})
	v = call[VoiceOut](t, cs, "voice_get", map[string]any{"run_id": run})
	if v.Personal == nil || !v.Personal.UseTeam || !strings.Contains(v.Guidance, "takes precedence") || !strings.Contains(v.Guidance, "No emojis") {
		t.Errorf("composed = %+v", v)
	}
	call[VoiceOut](t, cs, "voice_set", map[string]any{"description": "Direct and warm.", "use_team_default": false})
	if v = call[VoiceOut](t, cs, "voice_get", map[string]any{"run_id": run}); strings.Contains(v.Guidance, "Team style") {
		t.Errorf("opt-out ignored:\n%s", v.Guidance)
	}
	call[OKOut](t, cs, "voice_clear", map[string]any{})
	if v = call[VoiceOut](t, cs, "voice_get", map[string]any{}); v.Personal != nil {
		t.Error("voice not cleared")
	}
}
