// SPDX-License-Identifier: MIT

// Command aireviewer-mcp is the MCP server behind the aireviewer Claude Code
// plugin. It speaks MCP over stdio by default.
//
//	aireviewer-mcp                 # serve MCP over stdio
//	aireviewer-mcp inspect <url>   # print PR/MR metadata + diff stats (debugging)
//	aireviewer-mcp diffmap <url>   # check out the PR and print its semantic diff-map (debugging)
//	aireviewer-mcp augment <url>   # run the deterministic augmentation tools: issues, risk, refs, reviewers
//	aireviewer-mcp version
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"text/tabwriter"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/husobee/aireviewer/internal/forge"
	"github.com/husobee/aireviewer/internal/gitrepo"
	"github.com/husobee/aireviewer/internal/mcpserver"
	"github.com/husobee/aireviewer/internal/providers"
	"github.com/husobee/aireviewer/internal/runctx"
	"github.com/husobee/aireviewer/internal/sandbox"
	"github.com/husobee/aireviewer/internal/voice"
)

// version is set by -ldflags at release time.
var version = "dev"

func main() {
	log.SetFlags(0)
	log.SetPrefix("aireviewer-mcp: ")
	root := flag.String("root", "", "repository working tree (default: $CLAUDE_PROJECT_DIR, else current directory)")
	flag.Parse()

	if *root == "" {
		*root = os.Getenv("CLAUDE_PROJECT_DIR")
	}
	if *root == "" {
		wd, err := os.Getwd()
		if err != nil {
			log.Fatal(err)
		}
		*root = wd
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	hosts := forge.HostConfigFromEnv()
	reg := providers.NewRegistry()

	switch flag.Arg(0) {
	case "version":
		fmt.Println(version)
		return
	case "inspect":
		if flag.NArg() != 2 {
			log.Fatal("usage: aireviewer-mcp inspect <pr-or-mr-url>")
		}
		if err := inspect(ctx, hosts, reg, flag.Arg(1)); err != nil {
			log.Fatal(err)
		}
		return
	case "", "serve", "diffmap", "augment":
	default:
		log.Fatalf("unknown command %q", flag.Arg(0))
	}

	cache, err := gitrepo.CacheDir()
	if err != nil {
		log.Fatal(err)
	}
	srv := mcpserver.New(mcpserver.Config{
		Version:   version,
		RepoRoot:  *root,
		Hosts:     hosts,
		Providers: reg,
		Store:     runctx.New(*root),
		Tokens:    reg.Tokens,
		CacheDir:  cache,
		Sandbox:   sandbox.Docker{},
		VoiceDir:  voice.DefaultDir(),
	})
	if cmd := flag.Arg(0); cmd == "diffmap" || cmd == "augment" {
		if flag.NArg() != 2 {
			log.Fatalf("usage: aireviewer-mcp [-root dir] %s <pr-or-mr-url>", cmd)
		}
		if err := printDiffmap(ctx, srv, flag.Arg(1), cmd == "augment"); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

// printDiffmap drives the real tools through an in-memory MCP client.
func printDiffmap(ctx context.Context, srv *mcp.Server, url string, augment bool) error {
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		return err
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "aireviewer-cli"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		return err
	}
	defer func() { _ = cs.Close() }()
	callTool := func(name string, args map[string]any, out any) error {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			return err
		}
		if res.IsError {
			for _, c := range res.Content {
				if t, ok := c.(*mcp.TextContent); ok {
					return fmt.Errorf("%s: %s", name, t.Text)
				}
			}
			return fmt.Errorf("%s failed", name)
		}
		raw, _ := json.Marshal(res.StructuredContent)
		return json.Unmarshal(raw, out)
	}
	var co mcpserver.CheckoutOut
	if err := callTool("pr_checkout", map[string]any{"url": url}, &co); err != nil {
		return err
	}
	var dm mcpserver.DiffmapOut
	if err := callTool("diffmap_build", map[string]any{"run_id": co.RunID}, &dm); err != nil {
		return err
	}
	fmt.Printf("%s %s — %s\n  worktree:   %s\n  merge base: %s\n  head:       %s\n  units:      %d in %d files\n\n",
		co.Abbrev, co.Short, co.Title, co.Worktree, co.MergeBase, co.HeadSHA, len(dm.Map.Units), len(dm.Map.Files))
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	for _, id := range dm.Map.Order {
		u, _ := dm.Map.Unit(id)
		test := ""
		if u.IsTest {
			test = "test"
		}
		fmt.Fprintf(tw, "  %s\t%s\t+%d −%d\t%s\t%s\n", u.Kind, u.Change, u.Additions, u.Deletions, test, id)
	}
	if err := tw.Flush(); err != nil || !augment {
		return err
	}

	var is mcpserver.IssuesOut
	if err := callTool("pr_issues", map[string]any{"run_id": co.RunID}, &is); err != nil {
		return err
	}
	fmt.Printf("\nLinked issues: %s\n", is.Linkage)
	for _, i := range is.Issues {
		fmt.Printf("  %s [%s, %s] %s\n", i.Ref, i.State, i.Source, i.Title)
	}
	if is.Note != "" {
		fmt.Printf("  note: %s\n", is.Note)
	}
	var rm mcpserver.RepoMetricsOut
	if err := callTool("repo_metrics", map[string]any{"run_id": co.RunID}, &rm); err != nil {
		return err
	}
	fmt.Printf("\nRisk: %d/100 (%s) %s\n  history: %d commits in the year before the merge base, %d total\n",
		rm.Risk.Score, rm.Risk.Confidence, rm.Risk.ConfidenceReason, rm.RepoCommits365, rm.RepoCommitsTotal)
	for i, f := range rm.Risk.Files {
		if i == 5 {
			break
		}
		s := rm.Files[f.Path]
		fmt.Printf("  %3d  %-50s commits365=%d bugfix=%d authors=%d hotspot=%.2f\n", f.Score, f.Path, s.Commits365, s.Bugfix365, s.Authors365, s.HotspotPercentile)
	}
	var sr mcpserver.SymbolRefsOut
	if err := callTool("symbol_refs", map[string]any{"run_id": co.RunID}, &sr); err != nil {
		return err
	}
	fmt.Printf("\nImpact (symbol references at head):\n")
	for i, s := range sr.Symbols {
		if i == 8 {
			break
		}
		d := ""
		if s.Dangling {
			d = "  DANGLING"
		}
		fmt.Printf("  %-28s %-9s fan-in=%d refs=%d%s  (%s)\n", s.Symbol, s.Change, s.RefFiles, s.RefCount, d, s.UnitID)
	}
	if len(sr.InterfaceFiles) > 0 {
		fmt.Printf("  interface files: %v\n", sr.InterfaceFiles)
	}
	var rv mcpserver.ReviewersOut
	if err := callTool("reviewers_suggest", map[string]any{"run_id": co.RunID}, &rv); err != nil {
		return err
	}
	fmt.Printf("\nReviewers (mode %s; assigned: %v):\n", rv.Mode, rv.AlreadyAssigned)
	for _, c := range rv.Candidates {
		fmt.Printf("  %.2f  %-36s exp=%.2f own=%.2f work=%.2f load=%d\n", c.Score, c.Identity,
			c.Factors["expertise"], c.Factors["ownership"], c.Factors["workload"], c.OpenLoad)
	}
	for _, n := range rv.Notes {
		fmt.Printf("  note: %s\n", n)
	}
	return nil
}

func inspect(ctx context.Context, hosts forge.HostConfig, reg *providers.Registry, url string) error {
	ref, err := hosts.ParsePRURL(url)
	if err != nil {
		return err
	}
	p, err := reg.Get(ctx, ref.Repo)
	if err != nil {
		return err
	}
	pr, err := p.GetPR(ctx, ref)
	if err != nil {
		return err
	}
	d, err := p.GetDiff(ctx, ref)
	if err != nil {
		return err
	}
	draft := ""
	if pr.Draft {
		draft = " (draft)"
	}
	fmt.Printf("%s %s — %s\n", ref.Forge.Abbrev(), ref, pr.Title)
	fmt.Printf("  state:    %s%s\n  author:   %s\n  branches: %s ← %s\n  head:     %s\n  url:      %s\n",
		pr.State, draft, pr.Author, pr.BaseBranch, pr.HeadBranch, pr.HeadSHA, pr.WebURL)
	fmt.Printf("  diff:     %d files, +%d −%d\n\n", d.Stats.Files, d.Stats.Additions, d.Stats.Deletions)
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	for _, f := range d.Files {
		name := f.Path
		if f.OldPath != "" {
			name = f.OldPath + " → " + f.Path
		}
		note := ""
		if f.Truncated {
			note = "(patch truncated)"
		}
		fmt.Fprintf(tw, "  %s\t+%d\t−%d\t%s\t%s\n", f.Status, f.Additions, f.Deletions, name, note)
	}
	return tw.Flush()
}
