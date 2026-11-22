package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/seth-el/bee"
)

func runMemoryInventory(args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("bee memory inventory", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	rootValues := multiStringFlag{}
	flags.Var(&rootValues, "root", "source root LABEL=PATH (repeatable; required)")
	outputPath := flags.String("output", "", "absolute output manifest path (default: stdout)")
	excludeValues := multiStringFlag{}
	flags.Var(&excludeValues, "exclude", "relative path/glob to exclude (repeatable)")
	maxFiles := flags.Int("max-files", bee.DefaultInventoryMaxFiles, "maximum files to inspect")
	maxFileBytes := flags.Int64("max-file-bytes", bee.DefaultInventoryMaxFileBytes, "maximum bytes per file")
	maxTotalBytes := flags.Int64("max-total-bytes", bee.DefaultInventoryMaxTotalBytes, "maximum included source bytes")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("memory inventory does not accept positional arguments")
	}
	if len(rootValues.values) == 0 {
		return errors.New("memory inventory requires at least one --root LABEL=PATH")
	}
	roots := make([]bee.MemoryInventoryRoot, 0, len(rootValues.values))
	for _, value := range rootValues.values {
		label, path, ok := strings.Cut(value, "=")
		if !ok || strings.TrimSpace(label) == "" || strings.TrimSpace(path) == "" {
			return fmt.Errorf("invalid --root %q; expected LABEL=PATH", value)
		}
		roots = append(roots, bee.MemoryInventoryRoot{Label: strings.TrimSpace(label), Path: filepath.Clean(strings.TrimSpace(path))})
	}
	manifest, err := bee.InventoryMemorySources(bee.MemoryInventoryOptions{Roots: roots, Excludes: excludeValues.values, MaxFiles: *maxFiles, MaxFileBytes: *maxFileBytes, MaxTotalBytes: *maxTotalBytes})
	if err != nil {
		return err
	}
	if *outputPath != "" {
		if err := bee.WriteMemoryInventory(*outputPath, manifest); err != nil {
			return err
		}
		_, err := fmt.Fprintf(output, "wrote read-only memory inventory manifest: %s\n", *outputPath)
		return err
	}
	return json.NewEncoder(output).Encode(manifest)
}

type multiStringFlag struct{ values []string }

func (flag *multiStringFlag) String() string { return strings.Join(flag.values, ",") }
func (flag *multiStringFlag) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("repeatable flag value must not be empty")
	}
	flag.values = append(flag.values, value)
	return nil
}
