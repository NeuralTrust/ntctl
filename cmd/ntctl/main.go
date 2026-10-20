// Command ntctl installs and operates the NeuralTrust platform on Kubernetes
// without helm or kubectl.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/NeuralTrust/ntctl/internal/api"
	"github.com/NeuralTrust/ntctl/internal/helm"
	"github.com/NeuralTrust/ntctl/internal/prompt"
)

const usage = `ntctl installs and operates the NeuralTrust platform on Kubernetes.

Usage:
  ntctl <command> [flags]

Commands:
  install     Check the cluster, create the Secrets, install, and print the DNS records to create;
              on an existing install, add products to it
  upgrade     Upgrade an existing install, keeping its values: shows release notes and every
              change first, stops on unsafe ones, rolls back if it does not become ready
  rollback    Return an install to an earlier revision, showing what changes first
  history     Show the revisions of an install
  adopt       Check that a plain-Helm install can be managed with ntctl, changing nothing
  status      Show readiness, problem pods and DNS records for an install
  doctor      Diagnose an install and say how to fix what is wrong
  support-bundle  Write a redacted .tar.gz for NeuralTrust support
  preflight   Check a cluster against what an install needs, without changing anything
  render      Print the manifests an install would apply (same output as helm template)
  version     Print the ntctl version and the latest chart version

Console:
  login       Sign in to the NeuralTrust console in the browser and choose an organization
  logout      Sign out and forget the saved tokens
  auth status Show who is signed in, and the selected organization
  orgs list   List the organizations you belong to
  config      Show or set the region and organization
  trustgate   List or create private TrustGate instances
  trustguard  List or create private TrustGuard instances

Signed in, install creates or reuses the private instances and fetches their
tokens itself.

Run "ntctl <command> -h" for the flags of a command.
`

// errPreflight marks a run stopped by failing checks; the details were printed.
var errPreflight = errors.New("preflight checks failed")

// exitInterrupted is the shell's code for a process ended by SIGINT (128 + 2).
const exitInterrupted = 130

func main() {
	slog.SetDefault(slog.New(helm.Logger()))
	// The first Ctrl+C cancels ctx: a question ends, a Helm wait stops and rolls
	// back. Once it has, the default handling is restored, so a second Ctrl+C
	// ends the process wherever the run is.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	var err error
	switch args[0] {
	case "version":
		err = runVersion(ctx, args[1:], stdout)
	case "render":
		err = runRender(ctx, args[1:], stdout)
	case "preflight":
		err = runPreflight(ctx, args[1:], stdout)
	case "status":
		err = runStatus(ctx, args[1:], stdout)
	case "rollback":
		err = runRollback(ctx, args[1:], stdin, stdout)
	case "history":
		err = runHistory(ctx, args[1:], stdout)
	case "adopt":
		err = runAdopt(ctx, args[1:], stdout)
	case "doctor":
		err = runDoctor(ctx, args[1:], stdout)
	case "support-bundle":
		err = runSupportBundle(ctx, args[1:], stdout)
	case "install":
		err = runInstall(ctx, args[1:], stdin, stdout)
	case "upgrade":
		err = runUpgrade(ctx, args[1:], stdin, stdout)
	case "login":
		err = runLogin(ctx, args[1:], stdout)
	case "logout":
		err = runLogout(ctx, args[1:], stdout)
	case "auth":
		err = runAuth(ctx, args[1:], stdout)
	case "orgs":
		err = runOrgs(ctx, args[1:], stdout)
	case "config":
		err = runConfig(ctx, args[1:], stdout)
	case "trustgate":
		err = runInstances(api.TrustGate)(ctx, args[1:], stdout)
	case "trustguard":
		err = runInstances(api.TrustGuard)(ctx, args[1:], stdout)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	return exitCode(ctx, err, stderr)
}

// exitCode reports a command's outcome on stderr and returns its exit code.
func exitCode(ctx context.Context, err error, stderr io.Writer) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errHelp):
		return 0
	case errors.Is(err, errPreflight):
		return 3
	case errors.Is(err, prompt.ErrInterrupted):
		// Interrupted at a question: every command asks before it changes anything.
		fmt.Fprintln(stderr, "\nInterrupted.")
		return exitInterrupted
	case ctx.Err() != nil:
		fmt.Fprintf(stderr, "Error: %v\nInterrupted.\n", err)
		return exitInterrupted
	default:
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
}
