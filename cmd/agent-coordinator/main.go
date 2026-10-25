package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"agent-coordinator/internal/coordinator"
	"agent-coordinator/internal/tui"
)

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }
func run(args []string, in *os.File, out, errOut io.Writer) int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	jsonMode := false
	for _, arg := range args {
		if arg == "--json" {
			jsonMode = true
		}
	}
	fail := func(err error) int {
		var action *coordinator.ActionError
		code := 1
		diag := coordinator.Diagnostic{Code: "operational_failure", Message: "coordinator operation failed; inspect doctor", Uncertain: false}
		if errors.As(err, &action) {
			code = action.ExitCode
			diag = action.Diagnostic
		}
		if jsonMode {
			_ = json.NewEncoder(out).Encode(coordinator.ErrorResult{Error: diag})
		} else {
			fmt.Fprintln(errOut, diag.Message)
		}
		return code
	}
	invalid := func(message string) int {
		return fail(&coordinator.ActionError{ExitCode: 2, Diagnostic: coordinator.Diagnostic{Code: "invalid_usage", Message: message}})
	}
	var communicationBody []byte
	if len(args) > 0 && args[0] == "_communication" {
		body, readErr := io.ReadAll(io.LimitReader(in, coordinator.MaxCommunicationRequestBytes+1))
		if readErr != nil || len(body) > coordinator.MaxCommunicationRequestBytes {
			return invalid("communication request exceeds size limit")
		}
		communicationBody = body
		if isCapabilityRequest(body) {
			return runCommunicationBody(ctx, nil, args, body, out, fail, invalid)
		}
	}
	if len(args) > 0 && args[0] == "_run" {
		if len(args) != 2 {
			return 2
		}
		if err := coordinator.RunBootstrap(ctx, args[1]); err != nil {
			fmt.Fprintln(errOut, "coordinator bootstrap failed")
			return 1
		}
		return 0
	}
	options := coordinator.Options{}
	if w, h, err := coordinator.TerminalSize(int(in.Fd())); err == nil {
		options.Width, options.Height = w, h
	} else if len(args) == 0 {
		return invalid("dashboard requires terminal input and output")
	}
	if len(args) == 0 {
		if _, _, err := coordinator.TerminalSize(int(os.Stdout.Fd())); err != nil {
			return invalid("dashboard requires terminal input and output")
		}
	}
	c, err := coordinator.Open(options)
	if err != nil {
		return fail(err)
	}
	if len(args) > 0 && args[0] == "_communication" {
		return runCommunicationBody(ctx, c, args, communicationBody, out, fail, invalid)
	}
	if len(args) > 0 && (strings.HasPrefix(args[0], "_") || args[0] == "send" || args[0] == "stop" || args[0] == "force-stop") {
		return runControl(ctx, c, args, in, out, errOut, fail, invalid)
	}
	if len(args) == 0 {
		if err := tui.Run(ctx, c); err != nil {
			return fail(err)
		}
		return 0
	}
	command := args[0]
	rest := args[1:]
	if command == "messages" {
		return runMessages(ctx, c, rest, out, fail, invalid)
	}
	name := ""
	switch command {
	case "start", "doctor", "enter", "observe", "continue", "repair-socket":
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return invalid("command requires NAME before options")
		}
		name = rest[0]
		rest = rest[1:]
	case "forget":
		if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
			name = rest[0]
			rest = rest[1:]
		}
	case "list":
	default:
		return invalid("usage: agent-coordinator [start|list|messages|doctor|enter|observe|continue|repair-socket|forget]")
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	wantsJSON, sessionInfo := false, false
	cwd, taskFile, id := "", "", ""
	switch command {
	case "start":
		flags.StringVar(&cwd, "cwd", "", "existing working directory")
		flags.StringVar(&taskFile, "task-file", "", "UTF-8 file or - for stdin")
		flags.BoolVar(&wantsJSON, "json", false, "JSON result")
	case "list", "doctor":
		flags.BoolVar(&wantsJSON, "json", false, "JSON result")
		flags.BoolVar(&sessionInfo, "session-info", false, "include validated session details")
	case "forget":
		flags.StringVar(&id, "id", "", "immutable job UUID")
	}
	if err := flags.Parse(rest); err != nil || flags.NArg() != 0 {
		return invalid("invalid command options")
	}
	if (command != "start" && command != "list" && command != "doctor") && jsonMode {
		return invalid("--json is supported by start/list/doctor only")
	}
	emit := func(value interface{}) int {
		if wantsJSON {
			if err := json.NewEncoder(out).Encode(value); err != nil {
				return 1
			}
		} else {
			switch facts := value.(type) {
			case coordinator.JobFacts:
				printFacts(out, facts)
			case coordinator.ListResult:
				for _, job := range facts.Jobs {
					printFacts(out, job)
				}
			}
		}
		return 0
	}
	switch command {
	case "start":
		if cwd == "" || taskFile == "" {
			return invalid("start requires --cwd PATH --task-file PATH|-")
		}
		var reader io.Reader = in
		if taskFile != "-" {
			file, err := os.Open(taskFile)
			if err != nil {
				return invalid("task file is unavailable")
			}
			defer file.Close()
			reader = file
		}
		task, err := coordinator.ReadTask(reader)
		if err != nil {
			return invalid("task must be nonblank UTF-8 and at most 1 MiB")
		}
		facts, err := c.Start(ctx, name, cwd, task)
		if err != nil {
			return fail(err)
		}
		facts.Session = nil
		return emit(facts)
	case "list":
		facts, err := c.List(ctx)
		if err != nil {
			return fail(err)
		}
		if !sessionInfo {
			for i := range facts.Jobs {
				facts.Jobs[i].Session = nil
			}
		}
		return emit(facts)
	case "doctor":
		facts, err := c.Doctor(ctx, name)
		if err != nil {
			return fail(err)
		}
		if !sessionInfo {
			facts.Session = nil
		}
		return emit(facts)
	case "enter", "observe":
		if _, _, err := coordinator.TerminalSize(int(in.Fd())); err != nil {
			return invalid("Enter/Observe require an ordinary terminal outside tmux")
		}
		cmd, err := c.Attach(ctx, name, command == "observe")
		if err != nil {
			return fail(err)
		}
		fmt.Fprintln(errOut, "Shared input: writable clients may type concurrently. F12 Return detaches only you.")
		if err := coordinator.RunTerminal(ctx, cmd); err != nil {
			return fail(err)
		}
		return 0
	case "continue", "repair-socket":
		if _, _, err := coordinator.TerminalSize(int(in.Fd())); err != nil {
			return invalid("signal actions require terminal confirmation")
		}
		fmt.Fprintf(errOut, "%s sends an OS signal to the verified job. Type %s to confirm: ", command, name)
		var answer string
		if _, err := fmt.Fscanln(in, &answer); err != nil || answer != name {
			return invalid("action not confirmed")
		}
		if command == "continue" {
			err = c.Continue(ctx, name)
		} else {
			err = c.RepairSocket(ctx, name)
		}
		if err != nil {
			return fail(err)
		}
		fmt.Fprintln(out, "Signal sent; recovery/readiness remains unverified. Inspect doctor.")
		return 0
	case "forget":
		if (name == "") == (id == "") {
			return invalid("forget requires NAME or --id UUID")
		}
		target := name
		if id != "" {
			target = id
		}
		if err := c.Forget(ctx, target, id != ""); err != nil {
			return fail(err)
		}
		fmt.Fprintln(out, "Retired eligible job resources; UUID fence retained.")
		return 0
	}
	return 2
}
func printFacts(out io.Writer, f coordinator.JobFacts) {
	fmt.Fprintf(out, "%s %s launch=%s terminal=%s clients=%d readiness=%s outcome=%s\n  %s\n", f.Name, f.ID, f.Launch, f.Terminal, f.Clients, f.PiReadiness, f.TaskOutcome, f.NextAction)
}
