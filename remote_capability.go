package bee

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"
)

type CommandTarget string

const (
	CommandTargetWorkspace CommandTarget = "workspace"
	CommandTargetLocal     CommandTarget = "local"
	CommandTargetSSH       CommandTarget = "ssh"
)

type RemoteCapabilityGrant struct {
	SessionID         SessionID `json:"session_id"`
	Host              string    `json:"host"`
	User              string    `json:"user"`
	Port              int       `json:"port"`
	AllowLocalHost    bool      `json:"allow_local_host,omitempty"`
	AllowSSH          bool      `json:"allow_ssh,omitempty"`
	AllowCommands     bool      `json:"allow_commands,omitempty"`
	AllowElevated     bool      `json:"allow_elevated,omitempty"`
	AllowStdinScripts bool      `json:"allow_stdin_scripts,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	ExpiresAt         time.Time `json:"expires_at"`
}

func (grant RemoteCapabilityGrant) Validate() error {
	if err := grant.ValidateShape(); err != nil {
		return err
	}
	if grant.SessionID == "" {
		return errors.New("remote capability session_id is required")
	}
	return nil
}

func (grant RemoteCapabilityGrant) ValidateShape() error {
	if err := validateRemoteHost(grant.Host); err != nil {
		return err
	}
	if err := validateRemoteUser(grant.User); err != nil {
		return err
	}
	if grant.Port == 0 {
		grant.Port = 22
	}
	if grant.Port < 1 || grant.Port > 65535 {
		return errors.New("remote capability port must be between 1 and 65535")
	}
	if !grant.AllowLocalHost && !grant.AllowSSH {
		return errors.New("remote capability must allow local or SSH commands")
	}
	if !grant.AllowCommands {
		return errors.New("remote capability must explicitly allow commands")
	}
	if grant.ExpiresAt.IsZero() || !grant.ExpiresAt.After(time.Now().UTC()) {
		return errors.New("remote capability must have a future expiry")
	}
	if !grant.CreatedAt.IsZero() && grant.ExpiresAt.Before(grant.CreatedAt) {
		return errors.New("remote capability expiry precedes creation")
	}
	return nil
}

func (grant RemoteCapabilityGrant) active(now time.Time) bool {
	return grant.AllowCommands && !grant.ExpiresAt.IsZero() && now.Before(grant.ExpiresAt)
}

func (grant RemoteCapabilityGrant) permits(request commandRequest, now time.Time) error {
	if !grant.active(now) {
		return errors.New("remote capability grant is expired or inactive")
	}
	target := request.Target
	if target == "" {
		target = CommandTargetWorkspace
	}
	if request.Elevated && !grant.AllowElevated {
		return errors.New("session grant does not allow elevated commands")
	}
	if request.Stdin != "" && !grant.AllowStdinScripts {
		return errors.New("session grant does not allow remote stdin scripts")
	}
	switch target {
	case CommandTargetLocal:
		if !grant.AllowLocalHost {
			return errors.New("session grant does not allow local host commands")
		}
	case CommandTargetSSH:
		if !grant.AllowSSH {
			return errors.New("session grant does not allow SSH commands")
		}
		request = grant.resolveSSHRequest(request)
		if request.Host != grant.Host || request.User != grant.User || request.Port != grant.Port {
			return errors.New("SSH target does not match the session grant")
		}
	case CommandTargetWorkspace:
		return nil
	default:
		return fmt.Errorf("unknown command target %q", target)
	}
	return nil
}

func (grant RemoteCapabilityGrant) resolveSSHRequest(request commandRequest) commandRequest {
	if request.Target == CommandTargetSSH {
		if request.Host == "" {
			request.Host = grant.Host
		}
		if request.User == "" {
			request.User = grant.User
		}
		if request.Port == 0 {
			request.Port = grant.Port
			if request.Port == 0 {
				request.Port = 22
			}
		}
	}
	return request
}

var remoteUserPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`)
var remoteHostPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,252}$`)

func validateRemoteUser(user string) error {
	if strings.EqualFold(user, "root") {
		return errors.New("remote SSH user root is not permitted")
	}
	if !remoteUserPattern.MatchString(user) {
		return fmt.Errorf("remote SSH user %q is invalid", user)
	}
	return nil
}

func validateRemoteHost(host string) error {
	if net.ParseIP(host) != nil {
		return nil
	}
	if !remoteHostPattern.MatchString(host) || strings.Contains(host, "..") {
		return fmt.Errorf("remote SSH host %q must be an IP address or safe hostname", host)
	}
	return nil
}

func validateRemoteArg(value string) error {
	if value == "" {
		return errors.New("remote command arguments must not be empty")
	}
	for _, r := range value {
		if r == 0 || r < 0x20 || strings.ContainsRune(";&|$`<>\n\r", r) {
			return errors.New("remote command contains unsafe shell syntax")
		}
	}
	return nil
}

func renderRemoteCapabilityGrant(grant RemoteCapabilityGrant) string {
	return fmt.Sprintf("Active parent-owned SSH capability (temporary and not persisted): use target:%q for the exact remote %s@%s:%d. Omitted host, user, or port fields default from this grant only; an omitted target remains %q (workspace). SSH commands are allowed=%t; elevated commands are allowed=%t and use sudo -n; stdin scripts are allowed=%t. Do not infer another host, user, or port, and do not create or widen grants.", CommandTargetSSH, grant.User, grant.Host, grant.Port, CommandTargetWorkspace, grant.AllowSSH, grant.AllowElevated, grant.AllowStdinScripts)
}
