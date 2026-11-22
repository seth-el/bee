package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/seth-el/bee"
)

// runInbox reads and acknowledges the durable notification inbox that
// scheduled jobs write when they succeed, fail, or pause. Without it the
// inbox has no local reader, and a paused job is invisible unless someone
// opens the JSON by hand.
func runInbox(args []string, output, errorOutput io.Writer) error {
	if len(args) > 0 && args[0] == "ack" {
		return inboxAck(args[1:], output, errorOutput)
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") && args[0] != "list" {
		return fmt.Errorf("unknown inbox subcommand %q; usage: bee inbox [list] [--all] [--json] | bee inbox ack <id>|--all", args[0])
	}
	if len(args) > 0 && args[0] == "list" {
		args = args[1:]
	}
	flags := flag.NewFlagSet("bee inbox", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	storePath := flags.String("store", "", "private job store path (default: the configured or default job store)")
	configPath := flags.String("config", "", "Bee config path")
	all := flags.Bool("all", false, "include read notifications")
	limit := flags.Int("limit", 20, "maximum notifications to show (1-50)")
	jsonMode := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("bee inbox does not accept positional arguments")
	}
	if *limit < 1 || *limit > bee.NotificationMaxListLimit {
		return fmt.Errorf("--limit must be between 1 and %d", bee.NotificationMaxListLimit)
	}
	inbox, err := inboxStore(*storePath, *configPath)
	if err != nil {
		return err
	}
	ctx := context.Background()
	result, err := inbox.List(ctx, bee.NotificationMaxListLimit)
	if err != nil {
		return err
	}
	items := make([]bee.NotificationItemView, 0, len(result.Items))
	for _, item := range result.Items {
		if !*all && item.ReadAt != nil {
			continue
		}
		if len(items) == *limit {
			break
		}
		items = append(items, item)
	}
	if *jsonMode {
		result.Items = items
		return json.NewEncoder(output).Encode(result)
	}
	unread := fmt.Sprintf("%d", result.UnreadCount)
	if result.UnreadTruncated {
		unread += "+"
	}
	fmt.Fprintf(output, "inbox: %s unread\n", unread)
	if len(items) == 0 {
		if *all {
			fmt.Fprintln(output, "no notifications")
		} else {
			fmt.Fprintln(output, "nothing unread (use --all to include read notifications)")
		}
		return nil
	}
	now := time.Now()
	for _, item := range items {
		fmt.Fprintln(output, bee.FormatNotificationLine(item, now))
		if item.State == bee.NotificationStateFailed || item.State == bee.NotificationStatePaused {
			if reason, reasonErr := inbox.NotificationErrorFor(ctx, item.ID); reasonErr == nil && strings.TrimSpace(reason) != "" {
				fmt.Fprintf(output, "    reason: %s\n", strings.TrimSpace(reason))
			}
			fmt.Fprintf(output, "    next: bee jobs show --id %s | bee jobs retry --id %s | bee jobs dismiss --id %s\n", item.JobID, item.JobID, item.JobID)
		}
		fmt.Fprintf(output, "    id: %s\n", item.ID)
	}
	return nil
}

func inboxAck(args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("bee inbox ack", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	storePath := flags.String("store", "", "private job store path (default: the configured or default job store)")
	configPath := flags.String("config", "", "Bee config path")
	all := flags.Bool("all", false, "acknowledge every unread notification")
	id := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || (id == "") == !*all {
		return errors.New("usage: bee inbox ack <notification-id> | bee inbox ack --all")
	}
	inbox, err := inboxStore(*storePath, *configPath)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if *all {
		count, err := inbox.MarkAllRead(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(output, "acknowledged %d notification(s)\n", count)
		return nil
	}
	if _, err := inbox.MarkRead(ctx, id); err != nil {
		if errors.Is(err, bee.ErrNotificationNotFound) {
			return fmt.Errorf("no notification %q (ids are listed by `bee inbox`)", id)
		}
		return err
	}
	fmt.Fprintf(output, "acknowledged %s\n", id)
	return nil
}

// inboxStore opens the inbox that belongs to the job store, the same pairing
// `bee jobs run-due` and the composed assistant use.
func inboxStore(storePath, configPath string) (*bee.NotificationInboxStore, error) {
	jobs, err := resolveJobStore(storePath, configPath)
	if err != nil {
		return nil, err
	}
	return bee.NewNotificationInboxStore(bee.NotificationStorePathForJobStore(jobs.Path()))
}
