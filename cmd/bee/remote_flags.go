package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/seth-el/bee"
)

type remoteGrantFlags struct {
	enable   *bool
	host     *string
	user     *string
	port     *int
	elevated *bool
	scripts  *bool
	duration *time.Duration
}

func addRemoteGrantFlags(flags *flag.FlagSet) remoteGrantFlags {
	return remoteGrantFlags{
		enable:   flags.Bool("ssh-grant", false, "grant this session dynamic SSH command authority"),
		host:     flags.String("ssh-host", "", "SSH host/IP for the session grant; no alias registry"),
		user:     flags.String("ssh-user", "", "SSH username for the session grant"),
		port:     flags.Int("ssh-port", 22, "SSH port for the session grant"),
		elevated: flags.Bool("ssh-allow-elevated", false, "allow non-interactive sudo for this session grant"),
		scripts:  flags.Bool("ssh-allow-scripts", false, "allow bounded stdin scripts for this session grant"),
		duration: flags.Duration("ssh-grant-duration", 12*time.Hour, "session grant lifetime"),
	}
}

func (grantFlags remoteGrantFlags) build() (*bee.RemoteCapabilityGrant, error) {
	if !*grantFlags.enable {
		if strings.TrimSpace(*grantFlags.host) != "" || strings.TrimSpace(*grantFlags.user) != "" || *grantFlags.elevated || *grantFlags.scripts {
			return nil, errors.New("SSH grant options require --ssh-grant")
		}
		return nil, nil
	}
	if strings.TrimSpace(*grantFlags.host) == "" || strings.TrimSpace(*grantFlags.user) == "" {
		return nil, errors.New("--ssh-grant requires --ssh-host and --ssh-user")
	}
	if *grantFlags.duration <= 0 || *grantFlags.duration > 24*time.Hour {
		return nil, errors.New("--ssh-grant-duration must be between 1 minute and 24 hours")
	}
	now := time.Now().UTC()
	grant := &bee.RemoteCapabilityGrant{
		Host:              strings.TrimSpace(*grantFlags.host),
		User:              strings.TrimSpace(*grantFlags.user),
		Port:              *grantFlags.port,
		AllowSSH:          true,
		AllowCommands:     true,
		AllowElevated:     *grantFlags.elevated,
		AllowStdinScripts: *grantFlags.scripts,
		CreatedAt:         now,
		ExpiresAt:         now.Add(*grantFlags.duration),
	}
	if err := grant.ValidateShape(); err != nil {
		return nil, fmt.Errorf("SSH session grant: %w", err)
	}
	return grant, nil
}

func rejectRemoteGrantForNonTerminal(grant *bee.RemoteCapabilityGrant, jsonMode, rpcMode, tuiMode bool) error {
	if grant != nil && (jsonMode || rpcMode || tuiMode) {
		return errors.New("SSH session grants require the foreground terminal interface")
	}
	return nil
}

func printGrantNotice(output io.Writer, grant *bee.RemoteCapabilityGrant) error {
	if grant == nil {
		return nil
	}
	_, err := fmt.Fprintf(output, "bee SSH session grant active: %s@%s:%d; elevated=%t scripts=%t; expires=%s\n", grant.User, grant.Host, grant.Port, grant.AllowElevated, grant.AllowStdinScripts, grant.ExpiresAt.Format(time.RFC3339))
	return err
}
