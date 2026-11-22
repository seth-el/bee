package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/seth-el/bee"
	"github.com/seth-el/bee/adapters/telegram"
	telegramcompat "github.com/seth-el/bee/compat/telegram"
	"github.com/seth-el/bee/gateway"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "bee-telegram:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("bee-telegram", flag.ContinueOnError)
	configPath := flags.String("config", "", "path to the Bee JSON config")
	telegramConfigPath := flags.String("telegram-config", "", "path to the Telegram adapter JSON config")
	profile := flags.String("profile", "", "explicit provider profile; never auto-selects cloud")
	noBindings := flags.Bool("no-route-bindings", false, "disable durable route/session bindings")
	verifyOnly := flags.Bool("verify-only", false, "verify the configured bot identity with getMe and exit")
	inspectPending := flags.Bool("inspect-pending", false, "inspect pending update IDs without routing or acknowledging them")
	skipPending := flags.Bool("skip-pending", false, "acknowledge and skip all currently pending updates without routing them")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *telegramConfigPath == "" {
		return errors.New("--telegram-config is required")
	}
	beeConfig, err := bee.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	for _, line := range bee.UnknownConfigFieldWarnings(beeConfig) {
		fmt.Fprintln(os.Stderr, line)
	}
	adapterConfig, err := telegram.LoadConfig(*telegramConfigPath)
	if err != nil {
		return err
	}
	token, err := adapterConfig.Token()
	if err != nil {
		return err
	}
	sessionStore, err := bee.NewSessionStore(adapterConfig.SessionRoot)
	if err != nil {
		return err
	}
	app, err := composeTelegramAssistant(beeConfig, *profile, sessionStore)
	if err != nil {
		return err
	}
	defer func() { _ = app.Close(context.Background()) }()
	client, err := telegram.NewClient(adapterConfig.APIBaseURL, token, telegram.NewHTTPClient())
	if err != nil {
		return err
	}
	if *verifyOnly || *inspectPending || *skipPending {
		user, err := telegram.VerifyIdentity(context.Background(), client, adapterConfig)
		if err != nil {
			return err
		}
		if *verifyOnly {
			_, err = fmt.Fprintf(os.Stdout, "verified Telegram test bot @%s (%d)\n", user.Username, user.ID)
			return err
		}
		updates, err := client.GetUpdates(context.Background(), 0, 1, adapterConfig.PollLimit)
		if err != nil {
			return err
		}
		if *inspectPending {
			if _, err := fmt.Fprintf(os.Stdout, "pending Telegram updates: %d\n", len(updates)); err != nil {
				return err
			}
			for _, update := range updates {
				if _, err := fmt.Fprintf(os.Stdout, "update_id=%d\n", update.UpdateID); err != nil {
					return err
				}
			}
			return nil
		}
		checkpoint, err := telegram.NewCheckpointStore(adapterConfig.CheckpointPath)
		if err != nil {
			return err
		}
		nextOffset := int64(0)
		for _, update := range updates {
			if update.UpdateID >= nextOffset && update.UpdateID < telegram.MaxCheckpointOffset {
				nextOffset = update.UpdateID + 1
			}
		}
		if err := checkpoint.Save(context.Background(), nextOffset); err != nil {
			return err
		}
		_, err = fmt.Fprintf(os.Stdout, "skipped %d pending Telegram updates; next offset=%d\n", len(updates), nextOffset)
		return err
	}
	poller, err := telegramcompat.NewPoller(app, adapterConfig, telegramcompat.Options{NoRouteBindings: *noBindings, Logf: func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "bee-telegram: "+format+"\n", args...)
	}})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if app.Executor != nil {
		if err := app.StartExtensions(ctx); err != nil {
			return err
		}
	}
	go func() {
		menuCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := client.SetMyCommands(menuCtx, telegram.ChatCommandMenu(adapterConfig.VoiceEnabled())); err != nil {
			fmt.Fprintln(os.Stderr, "bee-telegram: command menu not published; slash commands still work")
		}
	}()
	if adapterConfig.NotifyChatID != 0 {
		store, err := notificationStoreFor(beeConfig)
		if err != nil {
			return fmt.Errorf("notification push: %w", err)
		}
		go telegram.RunNotificationPush(ctx, client, adapterConfig, store, telegram.DefaultNotifyInterval, func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "bee-telegram: "+format+"\n", args...)
		})
	}
	err = poller.Run(ctx)
	if err != nil && errors.Is(err, context.Canceled) && ctx.Err() != nil {
		// Stopped by SIGTERM or Ctrl-C: a clean shutdown, not a failure.
		return nil
	}
	return err
}

// notificationStoreFor opens the inbox the job runner writes: the one next to
// the configured job store, or the default.
func notificationStoreFor(config bee.Config) (*bee.NotificationInboxStore, error) {
	if jobStore := strings.TrimSpace(config.Assistant.JobStorePath); jobStore != "" {
		return bee.NewNotificationInboxStore(bee.NotificationStorePathForJobStore(filepath.Clean(jobStore)))
	}
	return bee.NewDefaultNotificationInboxStore()
}

// composeTelegramAssistant is the product composition root for the Telegram
// adapter. Keep it on the same general-assistant composition as cmd/bee so
// channels cannot accidentally drift into a reduced application variant.
func composeTelegramAssistant(config bee.Config, profile string, store *bee.SessionStore) (*bee.Application, error) {
	return gateway.ComposeAssistant(config, profile, store, bee.AssistantCompositionOptions{})
}
