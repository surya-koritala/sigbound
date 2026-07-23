package main

// `sig export` / `sig import` are thin wrappers over cell.Export / cell.Import —
// the two halves of git-bundle object transport for distributed group-folds. A
// WORKER runs `sig export` to bundle its agent branches into one file; that file
// moves by whatever means the user has (scp, shared dir, artifact store — no
// networking here); a COORDINATOR runs `sig import` to land those branches under
// an isolated imported/<worker>/ namespace, then feeds them to the ordinary `sig
// integrate` (they are just branch names). See docs/USAGE.md "Distributed
// workflow (bundles)".

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/surya-koritala/sigbound/cell"
)

// exportJSON is `sig export -json`'s stdout contract.
type exportJSON struct {
	Repo     string   `json:"repo"`
	Bundle   string   `json:"bundle"`
	Branches []string `json:"branches"`
}

func runExport(w io.Writer, argv []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: sig export -repo PATH -bundle FILE -branches b1,b2,.. [-json]")
		fs.PrintDefaults()
	}
	repo := fs.String("repo", "", "path to the source git repository (the worker's clone)")
	bundle := fs.String("bundle", "", "path of the bundle file to write")
	branchesCSV := fs.String("branches", "", "comma-separated branch names to export")
	asJSON := fs.Bool("json", false, "emit the result as JSON")
	if err := fs.Parse(argv); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if *repo == "" {
		return fmt.Errorf("-repo is required")
	}
	if *bundle == "" {
		return fmt.Errorf("-bundle is required")
	}
	branches := splitCSV(*branchesCSV)
	if len(branches) == 0 {
		return fmt.Errorf("-branches is required (comma-separated branch names)")
	}

	ctx := context.Background()
	c, err := cell.Open(*repo)
	if err != nil {
		return err
	}
	if err := c.Export(ctx, *bundle, branches); err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(exportJSON{Repo: *repo, Bundle: *bundle, Branches: branches})
	}
	fmt.Fprintf(w, "exported %d branch(es) to %s: %s\n", len(branches), *bundle, strings.Join(branches, ", "))
	return nil
}

// importedJSON is one landed branch in `sig import -json`'s output.
type importedJSON struct {
	Original string `json:"original"`
	Ref      string `json:"ref"`
	SHA      string `json:"sha"`
}

// importResultJSON is `sig import -json`'s stdout contract.
type importResultJSON struct {
	Repo     string         `json:"repo"`
	Bundle   string         `json:"bundle"`
	Imported []importedJSON `json:"imported"`
}

func runImport(w io.Writer, argv []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: sig import -repo PATH -bundle FILE [-from WORKER_ID] [-json]")
		fs.PrintDefaults()
	}
	repo := fs.String("repo", "", "path to the coordinator git repository")
	bundle := fs.String("bundle", "", "path of the bundle file to import")
	from := fs.String("from", "", "worker id namespace for imported branches (default: bundle filename stem)")
	asJSON := fs.Bool("json", false, "emit the result as JSON")
	if err := fs.Parse(argv); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if *repo == "" {
		return fmt.Errorf("-repo is required")
	}
	if *bundle == "" {
		return fmt.Errorf("-bundle is required")
	}

	ctx := context.Background()
	c, err := cell.Open(*repo)
	if err != nil {
		return err
	}
	imported, err := c.Import(ctx, *bundle, *from)
	if err != nil {
		return err
	}

	if *asJSON {
		out := importResultJSON{Repo: *repo, Bundle: *bundle, Imported: make([]importedJSON, 0, len(imported))}
		for _, ib := range imported {
			out.Imported = append(out.Imported, importedJSON{Original: ib.Original, Ref: ib.Ref, SHA: ib.SHA})
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	fmt.Fprintf(w, "imported %d branch(es) from %s:\n", len(imported), *bundle)
	for _, ib := range imported {
		fmt.Fprintf(w, "  %s -> %s (%s)\n", ib.Original, ib.Ref, ib.SHA)
	}
	return nil
}
