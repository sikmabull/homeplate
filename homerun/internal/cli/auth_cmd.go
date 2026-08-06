package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/homerun-ci/homerun/internal/auth"
	"github.com/homerun-ci/homerun/internal/ghapi"
)

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage GitHub identities (personal, work, client orgs)",
		Long: `Homerun supports multiple GitHub identities as named profiles.

  homerun auth add personal
  homerun auth add work --pat
  homerun auth list

Tokens are stored in the OS keychain (macOS Keychain / libsecret), never in
plaintext. One daemon multiplexes every identity's job queue on this machine.`,
	}
	cmd.AddCommand(newAuthAddCmd(), newAuthListCmd(), newAuthRemoveCmd())
	return cmd
}

func newAuthAddCmd() *cobra.Command {
	var (
		usePAT   bool
		clientID string
		host     string
		scopes   []string
	)
	cmd := &cobra.Command{
		Use:   "add <profile>",
		Short: "Authenticate a new GitHub identity",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			name := args[0]
			ctx, cancel := signalContext()
			defer cancel()

			st, err := auth.OpenStore()
			if err != nil {
				return err
			}

			var token string
			var kind auth.Kind
			var grantedScopes []string

			if usePAT {
				token, err = readSecret("Paste a GitHub token (fine-grained PAT recommended): ")
				if err != nil {
					return err
				}
				kind = auth.KindPAT
			} else {
				token, grantedScopes, err = runDeviceFlow(ctx, clientID, host, scopes)
				if err != nil {
					return err
				}
				kind = auth.KindDeviceFlow
			}
			if strings.TrimSpace(token) == "" {
				return fmt.Errorf("no token provided")
			}

			// Verify the token works BEFORE storing it, so a typo fails loudly
			// here instead of mysteriously at job time.
			client := ghapi.New(token)
			if host != "" {
				client = client.WithHost(host)
			}
			user, headerScopes, err := client.Whoami(ctx)
			if err != nil {
				if ghapi.IsUnauthorized(err) {
					return fmt.Errorf("GitHub rejected that token (401). Check it was copied in full and has not expired")
				}
				return fmt.Errorf("verifying token: %w", err)
			}
			if len(headerScopes) > 0 {
				grantedScopes = headerScopes
			}

			p := &auth.Profile{
				Name:     name,
				Login:    user.Login,
				Host:     hostOrDefault(host),
				Kind:     kind,
				Scopes:   grantedScopes,
				ClientID: clientID,
			}
			if err := st.Save(p, token); err != nil {
				return err
			}

			fmt.Printf("\nAuthenticated %s as @%s\n", name, user.Login)
			fmt.Printf("Token stored in: %s\n", st.KeyringName())
			if len(grantedScopes) > 0 {
				fmt.Printf("Scopes: %s\n", strings.Join(grantedScopes, ", "))
			} else {
				fmt.Println("Scopes: (fine-grained token; GitHub does not report scopes via header)")
			}
			warnMissingScopes(grantedScopes, kind)
			fmt.Printf("\nNext: homerun link --profile %s\n", name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&usePAT, "pat", false, "paste a personal access token instead of using device flow")
	cmd.Flags().StringVar(&clientID, "client-id", "", "OAuth App client_id for device flow")
	cmd.Flags().StringVar(&host, "host", "", "GitHub Enterprise Server hostname")
	cmd.Flags().StringSliceVar(&scopes, "scopes", nil, "override requested OAuth scopes")
	return cmd
}

// runDeviceFlow drives the interactive device authorization grant.
func runDeviceFlow(ctx context.Context, clientID, host string, scopes []string) (string, []string, error) {
	flow, err := auth.NewDeviceFlow(clientID, host, scopes)
	if err != nil {
		return "", nil, err
	}
	dc, err := flow.RequestCode(ctx)
	if err != nil {
		return "", nil, err
	}

	fmt.Println()
	fmt.Println("  1. Open:  " + dc.VerificationURI)
	fmt.Println("  2. Enter code:  " + dc.UserCode)
	fmt.Println()
	fmt.Printf("Waiting for authorization (expires in %s)...\n", time.Duration(dc.ExpiresIn)*time.Second)

	// Opening the browser is a convenience, never a requirement: the whole
	// point of device flow is that it works on a headless box.
	_ = openBrowser(dc.VerificationURI)

	token, granted, err := flow.Poll(ctx, dc, nil)
	if err != nil {
		return "", nil, err
	}
	return token, granted, nil
}

func warnMissingScopes(scopes []string, kind auth.Kind) {
	if kind != auth.KindDeviceFlow || len(scopes) == 0 {
		return
	}
	have := map[string]bool{}
	for _, s := range scopes {
		have[s] = true
	}
	if !have["repo"] {
		fmt.Println("\n  ! Missing `repo` scope: Homerun cannot register repo-level runners or post statuses.")
	}
	if !have["admin:org"] {
		fmt.Println("  ! Missing `admin:org` scope: org-level runner registration will fail (repo-level still works).")
	}
	if !have["workflow"] {
		fmt.Println("  ! Missing `workflow` scope: `homerun adopt` cannot open PRs that edit workflow files.")
	}
}

func newAuthListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured identities",
		RunE: func(c *cobra.Command, args []string) error {
			st, err := auth.OpenStore()
			if err != nil {
				return err
			}
			profiles := st.List()
			if len(profiles) == 0 {
				fmt.Println("No identities configured. Run: homerun auth add personal")
				return nil
			}
			fmt.Printf("Credential store: %s\n\n", st.KeyringName())
			fmt.Printf("%-14s %-20s %-14s %-12s %s\n", "PROFILE", "LOGIN", "HOST", "KIND", "SCOPES")
			for _, p := range profiles {
				sc := strings.Join(p.Scopes, ",")
				if sc == "" {
					sc = "(fine-grained)"
				}
				fmt.Printf("%-14s %-20s %-14s %-12s %s\n", p.Name, "@"+p.Login, p.Host, p.Kind, sc)
			}
			return nil
		},
	}
}

func newAuthRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <profile>",
		Short: "Delete an identity and its stored token",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			st, err := auth.OpenStore()
			if err != nil {
				return err
			}
			if err := st.Remove(args[0]); err != nil {
				return err
			}
			fmt.Printf("Removed profile %q and deleted its token from the keychain.\n", args[0])
			fmt.Println("Note: runners already registered on GitHub are ephemeral and expire on their own.")
			return nil
		},
	}
}

// readSecret reads a token without echoing it to the terminal.
func readSecret(prompt string) (string, error) {
	fmt.Print(prompt)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	// Piped input (scripts, tests): read a line.
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func hostOrDefault(h string) string {
	if h == "" {
		return "github.com"
	}
	return h
}

// clientFor resolves an authenticated client for a profile name, defaulting
// sensibly when only one identity exists.
func clientFor(profile string) (*ghapi.Client, *auth.Profile, error) {
	st, err := auth.OpenStore()
	if err != nil {
		return nil, nil, err
	}
	var p *auth.Profile
	if profile == "" {
		p, err = st.Default()
	} else {
		p, err = st.Get(profile)
	}
	if err != nil {
		return nil, nil, err
	}
	token, err := st.Token(p.Name)
	if err != nil {
		return nil, nil, err
	}
	client := ghapi.New(token)
	if p.Host != "" && p.Host != "github.com" {
		client = client.WithHost(p.Host)
	}
	client.Profile = p.Name
	return client, p, nil
}
