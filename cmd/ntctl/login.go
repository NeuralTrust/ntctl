package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/NeuralTrust/ntctl/internal/api"
	"github.com/NeuralTrust/ntctl/internal/auth"
	"github.com/NeuralTrust/ntctl/internal/config"
)

// httpClient talks to the console.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// openBrowser opens a URL in the user's browser. Tests replace it.
var openBrowser = func(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u) // #nosec G204 -- the console's own sign-in URL
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u) // #nosec G204 -- as above
	default:
		cmd = exec.Command("xdg-open", u) // #nosec G204 -- as above
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() //nolint:errcheck // only reaps the opener; the URL is printed too
	return nil
}

// session is a signed-in console: the saved config and tokens, and an API
// client that renews the access token as needed.
type session struct {
	cfg   *config.Config
	creds *auth.Credentials
	md    *auth.Metadata
	api   *api.Client
}

// openSession loads the saved login. It returns auth.ErrNotLoggedIn when
// there is none.
func openSession() (*session, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if cfg.AppURL == "" {
		return nil, auth.ErrNotLoggedIn
	}
	creds, err := auth.Load(cfg.AppURL)
	if err != nil {
		return nil, err
	}
	s := &session{cfg: cfg, creds: creds}
	s.api = api.New(cfg.AppURL, httpClient, s)
	return s, nil
}

// Token returns a usable access token, renewing it first when it expired.
func (s *session) Token(ctx context.Context) (string, error) {
	if s.creds.Valid() {
		return s.creds.AccessToken, nil
	}
	return s.Refresh(ctx)
}

// Refresh renews the access token and saves the rotated refresh token.
func (s *session) Refresh(ctx context.Context) (string, error) {
	if s.md == nil {
		md, err := auth.Discover(ctx, httpClient, s.cfg.AppURL)
		if err != nil {
			return "", err
		}
		s.md = md
	}
	t, err := auth.Refresh(ctx, httpClient, s.md, s.creds.RefreshToken)
	if err != nil {
		return "", err
	}
	s.creds.Token = *t
	if err := auth.Save(s.cfg.AppURL, s.creds); err != nil {
		return "", err
	}
	return t.AccessToken, nil
}

// org returns the organization to act on: the flag, else the configured one.
func (s *session) org(flagOrg string) (string, error) {
	if flagOrg != "" {
		return flagOrg, nil
	}
	if s.cfg.OrgID == "" {
		return "", errors.New("no organization selected; run `ntctl orgs list`, then `ntctl config set org <id>`")
	}
	return s.cfg.OrgID, nil
}

const loginSummary = `Sign in to the NeuralTrust console in the browser and choose an organization.
ntctl can then create private TrustGate and TrustGuard instances and fetch the
tokens an install needs, without the console UI.`

func runLogin(ctx context.Context, args []string, out io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	region := cfg.Region
	if region == "" {
		region = config.DefaultRegion
	}
	var appURL string
	var timeout time.Duration
	fs := plainFlags("login", loginSummary, out)
	fs.StringVar(&region, "region", region, "NeuralTrust region to sign in to: "+strings.Join(config.RegionNames(), " or "))
	fs.StringVar(&appURL, "app-url", "", "console URL, instead of the region's (for a dev or self-hosted console)")
	fs.DurationVar(&timeout, "timeout", 5*time.Minute, "how long to wait for the sign-in in the browser")
	if err := parse(fs, args); err != nil {
		return err
	}
	regionURL, err := config.AppURL(region)
	if err != nil {
		return err
	}
	if appURL == "" {
		appURL = regionURL
	}
	appURL = strings.TrimSuffix(appURL, "/")
	md, err := auth.Discover(ctx, httpClient, appURL)
	if err != nil {
		return err
	}
	tok, err := auth.Login(ctx, md, auth.LoginOptions{HTTP: httpClient, Open: openBrowser, Out: out, Timeout: timeout})
	if err != nil {
		return err
	}
	creds := &auth.Credentials{Token: *tok}
	s := &session{cfg: cfg, creds: creds, md: md}
	s.api = api.New(appURL, httpClient, s)
	me, err := s.api.Me(ctx)
	if err != nil {
		return err
	}
	creds.Email = me.Email
	if err := auth.Save(appURL, creds); err != nil {
		return err
	}
	if cfg.AppURL != appURL {
		cfg.OrgID, cfg.OrgName = "", ""
	}
	cfg.AppURL = appURL
	if me.Region != "" {
		cfg.Region = me.Region
	} else {
		cfg.Region = strings.ToLower(region)
	}
	if me.Org != nil {
		cfg.OrgID, cfg.OrgName = me.Org.ID, me.Org.Name
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Fprintf(out, "Signed in to %s as %s.\n", appURL, me.Email)
	if cfg.OrgID != "" {
		fmt.Fprintf(out, "Organization: %s (%s)\n", cfg.OrgName, cfg.OrgID)
	} else {
		fmt.Fprintln(out, "No organization selected; run `ntctl orgs list`, then `ntctl config set org <id>`.")
	}
	return nil
}

func runLogout(ctx context.Context, args []string, out io.Writer) error {
	fs := plainFlags("logout", "Sign out: end the session in the console and forget the saved tokens.", out)
	if err := parse(fs, args); err != nil {
		return err
	}
	s, err := openSession()
	if errors.Is(err, auth.ErrNotLoggedIn) {
		fmt.Fprintln(out, "Not signed in.")
		return nil
	}
	if err != nil {
		return err
	}
	// Forget the tokens even when the console cannot be reached; say so,
	// because the session then lives on until it expires.
	md, err := auth.Discover(ctx, httpClient, s.cfg.AppURL)
	if err == nil {
		err = auth.Revoke(ctx, httpClient, md, s.creds.RefreshToken)
	}
	if derr := auth.Delete(s.cfg.AppURL); derr != nil {
		return derr
	}
	if err != nil {
		fmt.Fprintf(out, "Forgot the saved tokens, but could not end the session in the console (%v); it expires on its own.\n", err)
		return nil
	}
	fmt.Fprintf(out, "Signed out of %s.\n", s.cfg.AppURL)
	return nil
}

func runAuth(ctx context.Context, args []string, out io.Writer) error {
	sub, rest := subcommand(args)
	if sub != "status" {
		return usageError("auth", "status", "Show who is signed in, to which console, and the selected organization.", out, sub)
	}
	fs := plainFlags("auth status", "Show who is signed in, to which console, and the selected organization.", out)
	if err := parse(fs, rest); err != nil {
		return err
	}
	s, err := openSession()
	if err != nil {
		return err
	}
	me, err := s.api.Me(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Console:      %s (region %s)\nSigned in as: %s\n", s.cfg.AppURL, s.cfg.Region, me.Email)
	if s.cfg.OrgID != "" {
		fmt.Fprintf(out, "Organization: %s (%s)\n", s.cfg.OrgName, s.cfg.OrgID)
	} else {
		fmt.Fprintln(out, "Organization: none selected")
	}
	return nil
}

func runOrgs(ctx context.Context, args []string, out io.Writer) error {
	sub, rest := subcommand(args)
	if sub != "list" {
		return usageError("orgs", "list", "List the organizations you belong to.", out, sub)
	}
	fs := plainFlags("orgs list", "List the organizations you belong to. * marks the selected one.", out)
	if err := parse(fs, rest); err != nil {
		return err
	}
	s, err := openSession()
	if err != nil {
		return err
	}
	orgs, err := s.api.Orgs(ctx)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "\tID\tNAME\tREGION\tROLE")
	for _, o := range orgs {
		mark := ""
		if o.ID == s.cfg.OrgID {
			mark = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", mark, o.ID, o.Name, o.Region, o.Role)
	}
	return tw.Flush()
}

const configSummary = `Show or change ntctl's defaults.

  ntctl config get                 show the console, region and organization
  ntctl config set org <id|name>   act on another organization you belong to
  ntctl config set region <eu|us>  the region ntctl login signs in to`

func runConfig(ctx context.Context, args []string, out io.Writer) error {
	sub, rest := subcommand(args)
	fs := plainFlags("config", configSummary, out)
	pos, err := parseArgs(fs, rest)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	switch {
	case sub == "get" && len(pos) == 0:
		fmt.Fprintf(out, "console: %s\nregion:  %s\norg:     %s %s\n", cfg.AppURL, cfg.Region, cfg.OrgID, parens(cfg.OrgName))
		return nil
	case sub == "set" && len(pos) == 2 && pos[0] == "region":
		u, err := config.AppURL(pos[1])
		if err != nil {
			return err
		}
		// Signed in, the region is the console's: changing it alone would
		// install data planes for one region with another's tokens.
		if s, err := openSession(); err == nil && s.cfg.AppURL != u {
			return fmt.Errorf("you are signed in to %s, and the region follows the console you sign in to; run `ntctl login --region %s` to switch", s.cfg.AppURL, strings.ToLower(pos[1]))
		}
		cfg.Region = strings.ToLower(pos[1])
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Fprintf(out, "Region set to %s. Run `ntctl login` to sign in there; each region has its own accounts.\n", cfg.Region)
		return nil
	case sub == "set" && len(pos) == 2 && pos[0] == "org":
		s, err := openSession()
		if err != nil {
			return err
		}
		orgs, err := s.api.Orgs(ctx)
		if err != nil {
			return err
		}
		o, err := findOrg(orgs, pos[1])
		if err != nil {
			return err
		}
		s.cfg.OrgID, s.cfg.OrgName = o.ID, o.Name
		if err := s.cfg.Save(); err != nil {
			return err
		}
		fmt.Fprintf(out, "Organization set to %s (%s).\n", o.Name, o.ID)
		return nil
	}
	fs.Usage()
	return errors.New("usage: ntctl config get | set org <id|name> | set region <eu|us>")
}

// findOrg returns the organization whose ID is key or, failing that, the one
// whose name is key, ignoring case; a name several share is refused.
func findOrg(orgs []api.Org, key string) (api.Org, error) {
	var named []api.Org
	for _, o := range orgs {
		if o.ID == key {
			return o, nil
		}
		if strings.EqualFold(o.Name, key) {
			named = append(named, o)
		}
	}
	switch len(named) {
	case 0:
		return api.Org{}, fmt.Errorf("you are not a member of an organization %q; see `ntctl orgs list`", key)
	case 1:
		return named[0], nil
	}
	ids := make([]string, 0, len(named))
	for _, o := range named {
		ids = append(ids, o.ID)
	}
	return api.Org{}, fmt.Errorf("%d of your organizations are named %q (%s); set one by its ID", len(named), key, strings.Join(ids, ", "))
}

func parens(s string) string {
	if s == "" {
		return ""
	}
	return "(" + s + ")"
}

// runInstances serves `ntctl trustgate` and `ntctl trustguard`.
func runInstances(p api.Product) func(context.Context, []string, io.Writer) error {
	return func(ctx context.Context, args []string, out io.Writer) error {
		name := strings.ToLower(p.Name())
		sub, rest := subcommand(args)
		var org string
		var summary string
		switch sub {
		case "list":
			summary = "List the organization's private " + p.Name() + " instances."
		case "create":
			summary = "Create a private " + p.Name() + " instance. `ntctl install` then fetches its tokens.\n\nUsage:\n  ntctl " + name + " create <name>"
		default:
			return usageError(name, "list|create", "Manage private "+p.Name()+" instances.", out, sub)
		}
		fs := plainFlags(name+" "+sub, summary, out)
		fs.StringVar(&org, "org", "", "organization ID (default: the selected one)")
		pos, err := parseArgs(fs, rest)
		if err != nil {
			return err
		}
		s, err := openSession()
		if err != nil {
			return err
		}
		if org, err = s.org(org); err != nil {
			return err
		}
		if sub == "create" {
			if len(pos) != 1 || strings.TrimSpace(pos[0]) == "" {
				return fmt.Errorf("usage: ntctl %s create <name>", name)
			}
			in, err := s.api.Create(ctx, p, org, strings.TrimSpace(pos[0]))
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Created private %s %q (%s). Run `ntctl install` to install it.\n", p.Name(), in.Name, in.ID)
			return nil
		}
		if len(pos) != 0 {
			return fmt.Errorf("unexpected arguments: %s", strings.Join(pos, " "))
		}
		list, err := s.api.Instances(ctx, p, org)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Fprintf(out, "No private %s instances. Create one with `ntctl %s create <name>`, or let `ntctl install` create it.\n", p.Name(), name)
			return nil
		}
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tENDPOINT")
		for _, in := range list {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", in.ID, in.Name, in.Endpoint)
		}
		return tw.Flush()
	}
}

// plainFlags is a flag set for commands that do not touch the cluster.
func plainFlags(name, summary string, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprintf(out, "%s\n\nFlags:\n", summary)
		fs.PrintDefaults()
	}
	return fs
}

// subcommand splits `ntctl orgs list --x` into "list" and the rest.
func subcommand(args []string) (string, []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", args
	}
	return args[0], args[1:]
}

// parseArgs parses flags placed before, between or after positional
// arguments, and returns the positional ones.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, errHelp
			}
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func usageError(cmd, subs, summary string, out io.Writer, got string) error {
	if got == "" || got == "help" {
		fmt.Fprintf(out, "%s\n\nUsage:\n  ntctl %s %s [flags]\n", summary, cmd, subs)
		return errHelp
	}
	return fmt.Errorf("unknown subcommand %q; usage: ntctl %s %s", got, cmd, subs)
}
