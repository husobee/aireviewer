// SPDX-License-Identifier: MIT

// Command aireviewer-mcp is the MCP server behind the aireviewer Claude Code
// plugin. It speaks MCP over stdio by default.
//
//	aireviewer-mcp                 # serve MCP over stdio
//	aireviewer-mcp inspect <url>   # print PR/MR metadata + diff stats (debugging)
//	aireviewer-mcp version
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"text/tabwriter"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/husobee/aireviewer/internal/forge"
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
	case "", "serve":
	default:
		log.Fatalf("unknown command %q", flag.Arg(0))
	}

	srv := mcpserver.New(mcpserver.Config{
		Version:   version,
		RepoRoot:  *root,
		Hosts:     hosts,
		Providers: reg,
		Store:     runctx.New(*root),
	})
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
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
