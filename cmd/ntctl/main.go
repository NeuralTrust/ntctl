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

	"github.com/NeuralTrust/ntctl/internal/helm"
)

const usage = `ntctl installs and operates the NeuralTrust platform on Kubernetes.

Usage:
  ntctl <command> [flags]

Commands:
  install     Check the cluster, create the Secrets, install, and print the DNS records to create
  upgrade     Upgrade an existing install, keeping its values; rolls back if it does not become ready
  status      Show readiness, problem pods and DNS records for an install
  preflight   Check a cluster against what an install needs, without changing anything
  render      Print the manifests an install would apply (same output as helm template)
  version     Print the ntctl version and the latest chart version

Run "ntctl <command> -h" for the flags of a command.
`

// errPreflight marks a run stopped by failing checks; the details were printed.
var errPreflight = errors.New("preflight checks failed")

func main() {
	slog.SetDefault(slog.New(helm.Logger()))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
	case "install":
		err = runInstall(ctx, args[1:], stdin, stdout)
	case "upgrade":
		err = runUpgrade(ctx, args[1:], stdin, stdout)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errHelp):
		return 0
	case errors.Is(err, errPreflight):
		return 3
	default:
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
}
