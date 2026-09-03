// Command cloud-echo scans an AWS account and reproduces it locally.
//
// Only `scan` talks to AWS, and it can only read. See
// docs/adr/0006-read-only-by-construction.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/matheusrcd/cloud-echo/internal/version"
)

// command is one CLI subcommand. Keeping this a plain dispatcher rather than
// pulling in a framework is a deliberate, reversible choice: it costs about
// sixty lines and keeps the dependency tree small enough that a security team
// can read all of it. Revisit if flag handling starts to hurt.
type command struct {
	name    string
	summary string
	run     func(ctx context.Context, args []string) error
}

func commands() []command {
	return []command{
		{"scan", "Read an AWS account into .cloud-echo/inventory.json", runScan},
		{"version", "Print the build identifier", runVersion},
	}
}

func main() {
	// A scan can be long. Ctrl-C should cancel it cleanly rather than leaving a
	// half-written inventory behind.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "cloud-echo: cancelled")
			os.Exit(130)
		}
		fmt.Fprintf(os.Stderr, "cloud-echo: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("no command given")
	}

	switch args[0] {
	case "-h", "--help", "help":
		usage()
		return nil
	case "-v", "--version":
		return runVersion(ctx, nil)
	}

	for _, c := range commands() {
		if c.name == args[0] {
			return c.run(ctx, args[1:])
		}
	}

	usage()
	return fmt.Errorf("unknown command %q", args[0])
}

func usage() {
	fmt.Fprintf(os.Stderr, "cloud-echo %s\n\nUsage:\n  cloud-echo <command> [flags]\n\nCommands:\n",
		version.String())
	for _, c := range commands() {
		fmt.Fprintf(os.Stderr, "  %-10s %s\n", c.name, c.summary)
	}
	fmt.Fprintf(os.Stderr, "\nRun `cloud-echo <command> -h` for flags.\n")
}

func runVersion(_ context.Context, _ []string) error {
	fmt.Println(version.String())
	return nil
}
