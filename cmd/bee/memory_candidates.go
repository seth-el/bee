package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/seth-el/bee"
)

func runMemoryCandidates(args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("bee memory candidates", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	inventoryPath := flags.String("inventory", "", "M1 memory inventory manifest path")
	candidatePath := flags.String("output", "", "absolute candidate JSONL output path (default: stdout)")
	reportPath := flags.String("report", "", "absolute report output path (required with --output)")
	types := flags.String("source-types", "", "comma-separated source types (default: daily_note; curated_long_term and curated_topic are derived layers and must be named explicitly)")
	includeArchives := flags.Bool("include-archives", false, "include archive source files")
	maxFiles := flags.Int("max-files", bee.DefaultCandidateMaxFiles, "maximum source files")
	maxSections := flags.Int("max-sections", bee.DefaultCandidateMaxSections, "maximum extracted sections")
	maxSectionBytes := flags.Int64("max-section-bytes", bee.DefaultCandidateMaxSectionBytes, "maximum section bytes")
	maxTotalBytes := flags.Int64("max-total-bytes", bee.DefaultCandidateMaxTotalBytes, "maximum candidate content bytes")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("memory candidates does not accept positional arguments")
	}
	if strings.TrimSpace(*inventoryPath) == "" {
		return errors.New("memory candidates requires --inventory")
	}
	var sourceTypes []string
	if strings.TrimSpace(*types) != "" {
		for _, value := range strings.Split(*types, ",") {
			if strings.TrimSpace(value) == "" {
				return errors.New("--source-types contains an empty value")
			}
			sourceTypes = append(sourceTypes, strings.TrimSpace(value))
		}
	}
	candidates, report, err := bee.ExtractMemoryCandidates(bee.MemoryCandidateOptions{InventoryPath: *inventoryPath, SourceTypes: sourceTypes, IncludeArchives: *includeArchives, MaxFiles: *maxFiles, MaxSections: *maxSections, MaxSectionBytes: *maxSectionBytes, MaxTotalBytes: *maxTotalBytes})
	if err != nil {
		return err
	}
	if *candidatePath == "" {
		return json.NewEncoder(output).Encode(struct {
			Report     bee.MemoryCandidateReport `json:"report"`
			Candidates []bee.MemoryCandidate     `json:"candidates"`
		}{report, candidates})
	}
	if *reportPath == "" {
		return errors.New("--report is required with --output")
	}
	if err := bee.WriteMemoryCandidates(*candidatePath, *reportPath, candidates, report); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "wrote read-only memory candidates: %s\nreport: %s\n", *candidatePath, *reportPath)
	return err
}
