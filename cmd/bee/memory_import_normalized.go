package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/seth-el/bee"
)

func memoryImportNormalized(args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("bee memory import-normalized", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	normalized := flags.String("normalized", "", "M3 normalized JSONL path")
	report := flags.String("report", "", "M3 review report path")
	memoryRoot := flags.String("memory-root", "", "private Bee memory root")
	auditID := flags.String("audit-id", "", "explicit import audit ID")
	reason := flags.String("reason", "", "explicit import reason")
	apply := flags.Bool("apply", false, "apply after complete validation; default is dry-run")
	receipt := flags.String("receipt", "", "optional owner-only report/receipt path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("memory import-normalized does not accept positional arguments")
	}
	if *normalized == "" || *report == "" || *memoryRoot == "" || *auditID == "" || *reason == "" {
		return errors.New("memory import-normalized requires --normalized, --report, --memory-root, --audit-id, and --reason")
	}
	result, err := bee.ImportNormalizedMemory(context.Background(), bee.NormalizedImportOptions{NormalizedPath: *normalized, ReviewReportPath: *report, MemoryRoot: *memoryRoot, AuditID: *auditID, Reason: *reason, Apply: *apply})
	if *receipt != "" {
		if receiptErr := bee.WriteNormalizedImportReceipt(*receipt, result); receiptErr != nil && err == nil {
			return receiptErr
		}
	}
	if encodeErr := json.NewEncoder(output).Encode(result); encodeErr != nil {
		return encodeErr
	}
	if err != nil {
		return err
	}
	if !result.Valid {
		return fmt.Errorf("normalized memory import is invalid")
	}
	return nil
}
