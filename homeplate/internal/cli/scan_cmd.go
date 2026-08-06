package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"

	"github.com/homeplate-ci/homeplate/internal/config"
	"github.com/homeplate-ci/homeplate/internal/repofind"
)

// scanRow is one discovered working copy plus its link state, for both the
// table and --json output.
type scanRow struct {
	Slug      string `json:"slug"`
	Path      string `json:"path"`
	Host      string `json:"host"`
	Branch    string `json:"branch"`
	Dirty     bool   `json:"dirty"`
	Linked    bool   `json:"linked"`
	LinkError string `json:"link_error,omitempty"`
	AdoptPR   string `json:"adopt_pr,omitempty"`
}

func newScanCmd() *cobra.Command {
	var (
		link     bool
		adopt    bool
		roots    []string
		depth    int
		allowPub bool
	)
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Discover local git working copies and optionally link+adopt them",
		Long: `Finds every git working copy under the standard roots (~/Documents, ~/code,
~/Projects, ...) whose origin remote is GitHub, and shows whether Homeplate
already serves it.

  homeplate scan                 just list what was found
  homeplate scan --link          link every discovered repo you administer
  homeplate scan --link --adopt  ... and open the runs-on PR for each

Linking a PUBLIC repository requires ` + PublicRepoFlag + `, same as ` + "`homeplate link`" + `.`,
		RunE: func(c *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()

			if len(roots) == 0 {
				roots = repofind.DefaultRoots()
			}
			found, err := repofind.Scan(ctx, roots, depth)
			if err != nil {
				return err
			}
			sort.Slice(found, func(i, j int) bool { return found[i].Slug < found[j].Slug })

			cfg, err := mustConfig()
			if err != nil {
				return err
			}

			rows := make([]scanRow, 0, len(found))
			for _, f := range found {
				_, linked := cfg.FindRepo(f.Slug)
				rows = append(rows, scanRow{
					Slug: f.Slug, Path: f.Path, Host: f.Host,
					Branch: f.Branch, Dirty: f.Dirty, Linked: linked,
				})
			}

			if link || adopt {
				if err := scanLinkAdopt(ctx, cfg, rows, link, adopt, allowPub); err != nil {
					return err
				}
			}

			if flagJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"roots": roots, "count": len(rows), "repos": rows,
				})
			}

			printScanTable(rows)
			if !link && !adopt {
				fmt.Println("\nNext: homeplate scan --link          link these repos")
				fmt.Println("      homeplate scan --link --adopt  link + open the runs-on PRs")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&link, "link", false, "link every discovered repo (private only, unless opted in)")
	cmd.Flags().BoolVar(&adopt, "adopt", false, "after linking, open the runs-on PR for each newly linked repo")
	cmd.Flags().StringSliceVar(&roots, "roots", nil, "comma-separated directories to scan (default: standard clone locations)")
	cmd.Flags().IntVar(&depth, "depth", 5, "how deep below each root to look for working copies")
	cmd.Flags().BoolVar(&allowPub, "i-understand-public-repo-risk", false,
		"allow linking PUBLIC repos (fork PRs = strangers' code on your machine)")
	return cmd
}

// scanLinkAdopt runs the link (and optionally adopt) flow over the discovered
// rows, updating each row's state in place. Per-repo failures are recorded on
// the row and never abort the batch.
func scanLinkAdopt(ctx context.Context, cfg *config.Config, rows []scanRow, doLink, doAdopt, allowPub bool) error {
	client, profile, err := clientFor(flagProfile)
	if err != nil {
		return err
	}
	changed := false
	for i := range rows {
		r := &rows[i]
		if r.Linked || !doLink {
			continue
		}
		sels, err := linkExplicit(ctx, client, profile, []string{r.Slug}, allowPub, "", nil)
		if err != nil {
			r.LinkError = oneLine(err)
			fmt.Fprintf(os.Stderr, "  ! %s: %s\n", r.Slug, oneLine(err))
			continue
		}
		for _, sel := range sels {
			sel.LocalPath = r.Path // we already know where it lives; skip the rescan
			cfg.AddRepo(sel)
			changed = true
			r.Linked = true
			if !flagJSON {
				fmt.Printf("  linked %s\n", r.Slug)
			}
		}
	}
	if changed {
		if err := cfg.Save(); err != nil {
			return err
		}
	}

	if doAdopt {
		for i := range rows {
			r := &rows[i]
			if !r.Linked || r.LinkError != "" {
				continue
			}
			url, err := adoptOne(ctx, client, r.Slug, adoptOptions{autoYes: true, quiet: flagJSON})
			if err != nil {
				fmt.Fprintf(os.Stderr, "  ! adopt %s: %s\n", r.Slug, oneLine(err))
				continue
			}
			r.AdoptPR = url
		}
	}
	return nil
}

func printScanTable(rows []scanRow) {
	if len(rows) == 0 {
		fmt.Println("No git working copies with GitHub remotes found.")
		fmt.Println("(scan looks under ~/Documents, ~/code, ~/Projects, ... - pass --roots to look elsewhere)")
		return
	}
	fmt.Println()
	fmt.Printf("  %-38s %-16s %-7s %-6s %s\n", "REPO", "BRANCH", "LINKED", "DIRTY", "PATH")
	for _, r := range rows {
		linked := ""
		if r.Linked {
			linked = "yes"
		}
		dirty := ""
		if r.Dirty {
			dirty = "yes"
		}
		if r.LinkError != "" {
			linked = "FAILED"
		}
		fmt.Printf("  %-38s %-16s %-7s %-6s %s\n",
			truncStr(r.Slug, 38), truncStr(r.Branch, 16), linked, dirty, r.Path)
		if r.AdoptPR != "" {
			fmt.Printf("  %-38s adopt PR: %s\n", "", r.AdoptPR)
		}
	}
	fmt.Printf("\n%d working copy(ies) found.\n", len(rows))
}
