// SPDX-License-Identifier: MIT

// Command aireviewer-mcp is the MCP server behind the aireviewer Claude Code
// plugin. It speaks MCP over stdio by default.
//
//	aireviewer-mcp                 # serve MCP over stdio
//	aireviewer-mcp inspect <url>   # print PR/MR metadata + diff stats (debugging)
//	aireviewer-mcp diffmap <url>   # check out the PR and print its semantic diff-map (debugging)
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
	case "", "serve", "diffmap":
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
	})
	if flag.Arg(0) == "diffmap" {
		if flag.NArg() != 2 {
			log.Fatal("usage: aireviewer-mcp [-root dir] diffmap <pr-or-mr-url>")
		}
		if err := printDiffmap(ctx, srv, flag.Arg(1)); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

// printDiffmap drives the real tools through an in-memory MCP client.
func printDiffmap(ctx context.Context, srv *mcp.Server, url string) error {
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
	return tw.Flush()
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
