package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/matheusrcd/cloud-echo/internal/inventory"
	"github.com/matheusrcd/cloud-echo/internal/linker"
	"github.com/matheusrcd/cloud-echo/internal/version"
)

// staleAfter is when an inventory is old enough to say so. graph never rescans
// on its own: a silent network call against a production account is a
// surprise, and surprises erode trust (docs/02-discovery.md).
const staleAfter = 24 * time.Hour

func runGraph(_ context.Context, args []string) error {
	return graphCmd(os.Stdout, os.Stderr, args, time.Now())
}

func graphCmd(stdout, stderr io.Writer, args []string, now time.Time) error {
	fs := flag.NewFlagSet("graph", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		invPath = fs.String("inventory", filepath.Join(".cloud-echo", "inventory.json"), "inventory written by `cloud-echo scan`")
		out     = fs.String("out", filepath.Join(".cloud-echo", "graph.json"), "where to write the graph")
		format  = fs.String("format", "text", "what to print: text | mermaid | json")
		explain = fs.Bool("explain", false, "print the evidence behind the edges between two nodes: --explain <from> <to>")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: cloud-echo graph [flags]\n       cloud-echo graph --explain <from> <to>\n\n"+
			"Infers what talks to what from the inventory. Offline: reads the inventory,\n"+
			"never AWS.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *explain && fs.NArg() != 2 {
		return errors.New("--explain takes two node ids: --explain <from> <to>")
	}

	f, err := os.Open(*invPath)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no inventory at %s — run `cloud-echo scan` first", *invPath)
	}
	if err != nil {
		return err
	}
	inv, err := inventory.Read(f)
	f.Close()
	if err != nil {
		return err
	}
	if age := now.Sub(inv.ScannedAt); age > staleAfter {
		fmt.Fprintf(stderr, "note: the inventory is %s old; `cloud-echo scan` refreshes it\n", age.Round(time.Hour))
	}

	g := linker.Link(inv, linker.Options{GeneratedBy: version.UserAgent()})
	if err := writeAtomic(*out, g.Write); err != nil {
		return err
	}

	if *explain {
		return linker.Explain(stdout, g, fs.Arg(0), fs.Arg(1))
	}
	switch *format {
	case "text":
		linker.WriteText(stdout, g)
		fmt.Fprintf(stderr, "\nwrote %s\n", *out)
	case "mermaid":
		linker.WriteMermaid(stdout, g)
	case "json":
		return g.Write(stdout)
	default:
		return fmt.Errorf("unknown format %q: text | mermaid | json", *format)
	}
	return nil
}
