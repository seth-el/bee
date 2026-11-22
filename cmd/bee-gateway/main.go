package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/seth-el/bee"
	"github.com/seth-el/bee/gateway"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "bee-gateway:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("bee-gateway", flag.ContinueOnError)
	configPath := flags.String("config", "", "path to the Bee JSON config")
	listenAddr := flags.String("listen", gateway.DefaultListenAddr, "listen address (loopback by default; 0.0.0.0 explicitly exposes LAN access)")
	profile := flags.String("profile", "", "explicit provider profile")
	scopeRoot := flags.String("scope-root", "", "assistant filesystem scope root")
	writeRoot := flags.String("write-root", "", "preferred automatically writable filesystem root")
	fullMachine := flags.Bool("full-machine", false, "explicitly allow full machine filesystem scope")
	policy := flags.String("policy", "", "assistant policy: trusted, prompted, or locked")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	beeConfig, err := bee.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	service, err := gateway.New(gateway.Config{
		BeeConfig:   beeConfig,
		Profile:     *profile,
		ListenAddr:  *listenAddr,
		ScopeRoot:   *scopeRoot,
		WriteRoot:   *writeRoot,
		FullMachine: *fullMachine,
		Policy:      bee.AssistantPolicyMode(*policy),
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := service.Start(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	return service.Stop(context.Background())
}
