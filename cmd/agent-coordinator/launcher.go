package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"agent-coordinator/internal/coordinator"
	"agent-coordinator/internal/pilaunch"
	"golang.org/x/sys/unix"
)

const nativeExecMarker = "AC_PI_EXEC_PID"

func runLaunchFeatures(args []string, out, errOut io.Writer) int {
	if len(args) != 2 || args[1] != "--json" {
		fmt.Fprintln(errOut, "usage: agent-coordinator _features --json")
		return 2
	}
	features := struct {
		Features struct {
			InteractiveLaunch int    `json:"interactive_launch"`
			TerminalProfile   string `json:"terminal_profile"`
			ManagerHost       int    `json:"manager_host"`
			PendingManager    int    `json:"pending_manager"`
		} `json:"features"`
	}{}
	features.Features.InteractiveLaunch = 1
	features.Features.TerminalProfile = coordinator.TransparentTerminalProfile
	features.Features.ManagerHost = 1
	features.Features.PendingManager = 1
	if err := json.NewEncoder(out).Encode(features); err != nil {
		return 1
	}
	return 0
}

func kittyEligible(in *os.File, out io.Writer) bool {
	output, ok := out.(*os.File)
	if !ok || os.Getenv("TERM") != "xterm-kitty" || os.Getenv("KITTY_WINDOW_ID") == "" || os.Getenv("TMUX") != "" {
		return false
	}
	_, inputErr := unix.IoctlGetWinsize(int(in.Fd()), unix.TIOCGWINSZ)
	_, outputErr := unix.IoctlGetWinsize(int(output.Fd()), unix.TIOCGWINSZ)
	return inputErr == nil && outputErr == nil
}

func runPi(ctx context.Context, args []string, in *os.File, out, errOut io.Writer) int {
	pid := strconv.Itoa(os.Getpid())
	if os.Getenv(nativeExecMarker) == pid {
		fmt.Fprintln(errOut, "native Pi launcher recursion refused; inspect the Pi executable on PATH")
		return 2
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(errOut, "coordinator executable identity unavailable")
		return 1
	}
	native, err := pilaunch.Resolve("pi", os.Getenv("PATH"), self)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	route := pilaunch.Classify(args, kittyEligible(in, out))
	if route.Kind == pilaunch.Passthrough {
		environment := make([]string, 0, len(os.Environ())+1)
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, nativeExecMarker+"=") {
				environment = append(environment, entry)
			}
		}
		environment = append(environment, nativeExecMarker+"="+pid)
		if err := unix.Exec(native, append([]string{native}, args...), environment); err != nil {
			fmt.Fprintln(errOut, "native Pi exec failed")
			return 1
		}
		return 0
	}
	if route.Kind == pilaunch.Refused {
		fmt.Fprintf(errOut, "%s; use command pi for native bypass without persistence guarantees\n", route.Reason)
		return 2
	}
	managed, err := pilaunch.ObserveManaged(native)
	if err != nil {
		fmt.Fprintf(errOut, "%s; use command pi for native bypass without persistence guarantees\n", err)
		return 2
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(errOut, "working directory unavailable")
		return 1
	}
	width, height, err := coordinator.TerminalSize(int(in.Fd()))
	if err != nil {
		fmt.Fprintln(errOut, "interactive Pi requires positive terminal geometry; resize the terminal and retry")
		return 2
	}
	c, err := coordinator.Open(coordinator.Options{PiPath: native, Executable: self, Width: width, Height: height})
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	facts, err := c.StartInteractive(ctx, cwd, args, managed)
	if err != nil {
		fmt.Fprintf(errOut, "interactive launch failed (job %s): %s; inspect list/doctor; do not retry automatically\n", facts.ID, err)
		return 1
	}
	command, attachErr := c.AttachInitialID(ctx, facts.ID)
	if attachErr == nil {
		attachErr = coordinator.RunTerminal(ctx, command)
	}
	outcome, outcomeErr := c.FinishInteractiveAttach(ctx, facts.ID, attachErr)
	if outcome.Transcript != "" {
		fmt.Fprintln(errOut, outcome.Transcript)
	}
	if outcomeErr != nil {
		fmt.Fprintf(errOut, "job %s: %s; inspect doctor before any new launch\n", facts.ID, outcomeErr)
		return 75
	}
	if outcome.ExitStatus != nil {
		return *outcome.ExitStatus
	}
	if attachErr != nil {
		fmt.Fprintf(errOut, "attachment failed; Pi may remain live as job %s; inspect list/doctor\n", facts.ID)
		return 1
	}
	fmt.Fprintf(errOut, "Detached from job %s; Pi remains live. Reconnect with agent-coordinator enter %s\n", facts.ID, facts.Name)
	return 0
}
