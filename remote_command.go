package bee

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

type commandRequest struct {
	Target   CommandTarget
	Host     string
	User     string
	Port     int
	Elevated bool
	Stdin    string
}

func executeLocalCommand(ctx context.Context, argv []string, stdin, dir string, scope ExecutionScope, artifactStores ...*ArtifactStore) (string, error) {
	var artifactStore *ArtifactStore
	if len(artifactStores) > 0 {
		artifactStore = artifactStores[0]
	}
	program, commandArgs, err := sandboxCommandInvocation(scope.withNetworkPolicy(ctx), argv[0], argv[1:], dir)
	if err != nil {
		return "", err
	}
	command := exec.CommandContext(ctx, program, commandArgs...)
	command.Dir = dir
	setAllowlistedChildEnvironment(command, scope.EnvAllowlist, scope.EnforceWriteRoots)
	if stdin != "" {
		command.Stdin = strings.NewReader(stdin)
	}
	retain := commandArtifactContextAvailable(ctx, artifactStore)
	output, capture, err := runCapturedCommand(command, scope.maxOutput(), retain)
	if capture != nil && output.Truncated {
		continuation := retainCommandOutput(ctx, artifactStore, capture)
		if continuation != nil {
			wrapped, wrapErr := marshalCommandOutputEnvelope(capture, output.Text, true, continuation, scope.maxOutput())
			if wrapErr == nil {
				return wrapped, err
			}
		}
	}
	return output.Text, err
}

func executeSSHCommand(ctx context.Context, input runArgs, scope ExecutionScope, artifactStores ...*ArtifactStore) (string, error) {
	var artifactStore *ArtifactStore
	if len(artifactStores) > 0 {
		artifactStore = artifactStores[0]
	}
	if scope.RemoteGrant == nil && !scope.TrustedForeground {
		return "", errors.New("SSH execution requires trusted foreground scope or a parent-owned session capability grant")
	}
	if scope.RemoteGrant != nil {
		resolved := scope.RemoteGrant.resolveSSHRequest(commandRequest{Target: CommandTargetSSH, Host: input.Host, User: input.User, Port: input.Port})
		input.Host, input.User, input.Port = resolved.Host, resolved.User, resolved.Port
	}
	if err := validateRemoteHost(input.Host); err != nil {
		return "", err
	}
	if err := validateRemoteUser(input.User); err != nil {
		return "", err
	}
	port := input.Port
	if port == 0 {
		port = 22
	}
	if port < 1 || port > 65535 {
		return "", errors.New("SSH port must be between 1 and 65535")
	}
	remoteArgv := append([]string(nil), input.Argv...)
	if input.Elevated && (len(remoteArgv) == 0 || remoteArgv[0] != "sudo") {
		remoteArgv = append([]string{"sudo", "-n"}, remoteArgv...)
	}
	sshArgv := []string{"ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-p", fmt.Sprint(port), input.User + "@" + input.Host}
	sshArgv = append(sshArgv, remoteArgv...)
	return executeLocalCommand(ctx, sshArgv, input.Stdin, "", scope, artifactStore)
}

// boundedCommandBuffer deliberately names its bytes.Buffer instead of
// embedding it. Embedding promotes bytes.Buffer.ReadFrom, which lets io.Copy
// bypass Write and defeat the capture limit.
type boundedCommandBuffer struct {
	data      bytes.Buffer
	limit     int
	truncated bool
}

func (buffer *boundedCommandBuffer) Write(data []byte) (int, error) {
	if buffer.limit <= 0 {
		buffer.truncated = buffer.truncated || len(data) > 0
		return len(data), nil
	}
	remaining := buffer.limit - buffer.Len()
	if remaining <= 0 {
		buffer.truncated = buffer.truncated || len(data) > 0
		return len(data), nil
	}
	if len(data) > remaining {
		_, _ = buffer.data.Write(data[:remaining])
		buffer.truncated = true
		return len(data), nil
	}
	_, _ = buffer.data.Write(data)
	return len(data), nil
}

func (buffer *boundedCommandBuffer) Len() int {
	return buffer.data.Len()
}

func (buffer *boundedCommandBuffer) String() string {
	return buffer.data.String()
}

type capturedCommandOutput struct {
	Text      string
	Truncated bool
}

func runCapturedCommand(command *exec.Cmd, maxOutput int, retain bool) (capturedCommandOutput, *commandCapture, error) {
	if maxOutput <= 0 {
		maxOutput = 64 << 10
	}
	capture := newCommandCapture(maxOutput, retain)
	command.Stdout = &capture.stdout
	command.Stderr = &capture.stderr
	err := command.Run()
	text, truncated := capture.visibleOutput(maxOutput)
	return capturedCommandOutput{Text: text, Truncated: truncated}, &capture, err
}

func runBoundedCommand(command *exec.Cmd, maxOutput int) (string, error) {
	output, _, err := runCapturedCommand(command, maxOutput, false)
	return output.Text, err
}
