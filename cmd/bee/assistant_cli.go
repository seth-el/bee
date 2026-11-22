package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/seth-el/bee"
)

// terminalApproval is used only when prompted policy is explicitly selected
// for an interactive foreground interface.
type terminalApproval struct {
	input  *bufio.Reader
	output io.Writer
}

func (approval terminalApproval) Approve(ctx context.Context, request bee.ApprovalRequest) (bee.ApprovalDecision, error) {
	if approval.input == nil || approval.output == nil {
		return bee.ApprovalDenied, errors.New("terminal approval input and output are required")
	}
	if err := ctx.Err(); err != nil {
		return bee.ApprovalDenied, err
	}
	prompt := "approve? [y/N]: "
	if request.SessionGrant != "" {
		prompt = fmt.Sprintf("approve? [y/N, s = allow writes under %s for this session]: ", request.SessionGrant)
	}
	if _, err := fmt.Fprintf(approval.output, "\nbee approval required\n  tool: %s\n  summary: %s\n  args: %s\n%s", request.ToolName, request.Summary, boundedApprovalArgs(request.Args), prompt); err != nil {
		return bee.ApprovalDenied, err
	}
	line, err := approval.input.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return bee.ApprovalDenied, err
	}
	if err := ctx.Err(); err != nil {
		return bee.ApprovalDenied, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer == "y" || answer == "yes" {
		if _, err := io.WriteString(approval.output, "approved\n"); err != nil {
			return bee.ApprovalDenied, err
		}
		return bee.ApprovalApproved, nil
	}
	if (answer == "s" || answer == "session") && request.SessionGrant != "" {
		if _, err := fmt.Fprintf(approval.output, "approved; writes under %s allowed for this session\n", request.SessionGrant); err != nil {
			return bee.ApprovalDenied, err
		}
		return bee.ApprovalApprovedForSession, nil
	}
	if _, err := io.WriteString(approval.output, "denied\n"); err != nil {
		return bee.ApprovalDenied, err
	}
	return bee.ApprovalDenied, nil
}

func boundedApprovalArgs(args json.RawMessage) string {
	var fields map[string]any
	if err := json.Unmarshal(args, &fields); err != nil {
		return "<invalid arguments>"
	}
	for _, key := range []string{"content", "old", "new", "stdin"} {
		if value, ok := fields[key].(string); ok {
			fields[key] = fmt.Sprintf("<redacted %d bytes>", len(value))
		}
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return "<unavailable arguments>"
	}
	const limit = 512
	text := string(data)
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "..."
}

type terminalAuditor struct{ output io.Writer }

func (auditor terminalAuditor) Record(_ context.Context, event bee.AuditEvent) error {
	if auditor.output == nil {
		return errors.New("terminal audit output is required")
	}
	if _, err := fmt.Fprintf(auditor.output, "bee audit: tool=%s decision=%s success=%t", event.ToolName, event.Decision, event.Success); err != nil {
		return err
	}
	if event.Error != "" {
		if _, err := fmt.Fprintf(auditor.output, " error=%s", event.Error); err != nil {
			return err
		}
	}
	_, err := io.WriteString(auditor.output, "\n")
	return err
}

func newFlagSet(name string, output io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(output)
	return flags
}
