package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"agent-coordinator/internal/coordinator"
)

func runControl(ctx context.Context, c *coordinator.Coordinator, args []string, in *os.File, out, errOut io.Writer, fail func(error) int, invalid func(string) int) int {
	command := args[0]
	if command == "_target" {
		if len(args) != 3 || args[2] != "--json" {
			return invalid("_target requires UUID --json")
		}
		target, err := c.ControlTarget(ctx, args[1])
		if err != nil {
			return fail(err)
		}
		return writeControlJSON(out, target)
	}
	if command == "_confirmed-stop" {
		if len(args) != 2 || args[1] != "--json" {
			return invalid("_confirmed-stop requires --json")
		}
		var auth coordinator.StopAuthorization
		body, err := io.ReadAll(io.LimitReader(in, 16*1024+1))
		if err != nil || len(body) > 16*1024 {
			return invalid("invalid stop authorization")
		}
		decoder := json.NewDecoder(strings.NewReader(string(body)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&auth); err != nil || decoder.Decode(new(any)) != io.EOF {
			return invalid("invalid stop authorization")
		}
		receipt, err := c.ConfirmStop(ctx, auth)
		if err != nil {
			return fail(err)
		}
		return writeControlJSON(out, receipt)
	}
	if command == "send" {
		if len(args) < 2 {
			return invalid("send requires UUID")
		}
		fs := flag.NewFlagSet(command, flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		session, generation, request, jsonOutput := "", "", "", false
		fs.StringVar(&session, "session-id", "", "native session ID")
		fs.StringVar(&generation, "generation", "", "runtime generation")
		fs.StringVar(&request, "request-id", "", "request UUID")
		fs.BoolVar(&jsonOutput, "json", false, "JSON result")
		if fs.Parse(args[2:]) != nil || fs.NArg() != 0 || !jsonOutput || session == "" || generation == "" || request == "" {
			return invalid("send requires --session-id ID --generation UUID --request-id UUID --json")
		}
		body, err := io.ReadAll(io.LimitReader(in, coordinator.MaxControlTextBytes+1))
		if err != nil || len(body) > coordinator.MaxControlTextBytes {
			return invalid("message exceeds UTF-8 size limit")
		}
		target, err := c.ControlTarget(ctx, args[1])
		if err != nil {
			return fail(err)
		}
		if target.SessionID != session || target.Generation != generation {
			return fail(sessionTargetChanged(args[1]))
		}
		receipt, err := c.Send(ctx, target, request, string(body))
		if err != nil {
			return fail(err)
		}
		return writeControlJSON(out, receipt)
	}
	if command == "_prepare-stop" {
		if len(args) < 2 {
			return invalid("_prepare-stop requires UUID")
		}
		fs := flag.NewFlagSet(command, flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		session, generation := "", ""
		force := false
		jsonOutput := false
		fs.StringVar(&session, "session-id", "", "native session ID")
		fs.StringVar(&generation, "generation", "", "runtime generation")
		fs.BoolVar(&force, "force", false, "authorize force-stop")
		fs.BoolVar(&jsonOutput, "json", false, "JSON result")
		if fs.Parse(args[2:]) != nil || fs.NArg() != 0 || !jsonOutput || session == "" || generation == "" {
			return invalid("_prepare-stop requires target, generation and --json")
		}
		target, err := c.ControlTarget(ctx, args[1])
		if err != nil {
			return fail(err)
		}
		if target.SessionID != session || target.Generation != generation {
			return fail(sessionTargetChanged(args[1]))
		}
		auth, err := c.PrepareStop(ctx, target, force)
		if err != nil {
			return fail(err)
		}
		return writeControlJSON(out, auth)
	}
	if command == "stop" || command == "force-stop" {
		if len(args) != 2 || strings.HasPrefix(args[1], "-") {
			return invalid(command + " requires UUID")
		}
		if _, _, err := coordinator.TerminalSize(int(in.Fd())); err != nil {
			return invalid("stop requires interactive terminal confirmation")
		}
		if _, _, err := coordinator.TerminalSize(int(os.Stdout.Fd())); err != nil {
			return invalid("stop requires interactive terminal confirmation")
		}
		target, err := c.ControlTarget(ctx, args[1])
		if err != nil {
			return fail(err)
		}
		force := command == "force-stop"
		if force {
			fmt.Fprintf(errOut, "SIGKILL targets only the verified foreground Pi group; detached children survive and writes may be incomplete. Type FORCE %s: ", target.ID)
		} else {
			fmt.Fprintf(errOut, "Request graceful shutdown of %s. Type STOP %s: ", target.ID, target.ID)
		}
		var word, answer string
		if _, err := fmt.Fscanln(in, &word, &answer); err != nil || word+" "+answer != map[bool]string{true: "FORCE " + target.ID, false: "STOP " + target.ID}[force] {
			return invalid("action not confirmed")
		}
		auth, err := c.PrepareStop(ctx, target, force)
		if err != nil {
			return fail(err)
		}
		receipt, err := c.ConfirmStop(ctx, auth)
		if err != nil {
			return fail(err)
		}
		return writeControlJSON(out, receipt)
	}
	return invalid("unknown control command")
}

func sessionTargetChanged(id string) error {
	return &coordinator.ActionError{ExitCode: 1, Diagnostic: coordinator.Diagnostic{Code: "identity_changed", Message: "session target changed; resolve it again", ID: id, Uncertain: false}}
}

func writeControlJSON(out io.Writer, value any) int {
	if err := json.NewEncoder(out).Encode(value); err != nil {
		return 1
	}
	return 0
}
