package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/seth-el/bee"
)

func runMemorySourceArchive(args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("bee memory source-archive", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	rootValues := multiStringFlag{}
	flags.Var(&rootValues, "root", "source root LABEL=PATH (repeatable; required)")
	excludeValues := multiStringFlag{}
	flags.Var(&excludeValues, "exclude", "relative path/glob to exclude (repeatable; Dreaming paths excluded by default)")
	name := flags.String("name", "", "archive name (defaults to the single root label)")
	destination := flags.String("destination", "", "absolute archive destination under BEE_HOME/workspace/source-memory")
	maxFiles := flags.Int("max-files", bee.DefaultSourceArchiveMaxFiles, "maximum files to archive")
	maxFileBytes := flags.Int64("max-file-bytes", bee.DefaultSourceArchiveMaxFileBytes, "maximum bytes per file")
	maxTotalBytes := flags.Int64("max-total-bytes", bee.DefaultSourceArchiveMaxTotalBytes, "maximum total source bytes")
	dryRun := flags.Bool("dry-run", false, "inspect and hash sources without writing an archive")
	overwrite := flags.Bool("overwrite", false, "replace an existing archive destination")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("memory source-archive does not accept positional arguments")
	}
	if len(rootValues.values) == 0 {
		return errors.New("memory source-archive requires at least one --root LABEL=PATH")
	}
	roots := make([]bee.MemoryInventoryRoot, 0, len(rootValues.values))
	for _, value := range rootValues.values {
		label, path, ok := strings.Cut(value, "=")
		if !ok || strings.TrimSpace(label) == "" || strings.TrimSpace(path) == "" {
			return fmt.Errorf("invalid --root %q; expected LABEL=PATH", value)
		}
		roots = append(roots, bee.MemoryInventoryRoot{Label: strings.TrimSpace(label), Path: filepath.Clean(strings.TrimSpace(path))})
	}
	report, err := bee.ArchiveMemorySources(bee.MemorySourceArchiveOptions{Roots: roots, Excludes: excludeValues.values, Name: *name, Destination: *destination, MaxFiles: *maxFiles, MaxFileBytes: *maxFileBytes, MaxTotalBytes: *maxTotalBytes, DryRun: *dryRun, Overwrite: *overwrite})
	if err != nil {
		return err
	}
	return encodeValue(output, report)
}
