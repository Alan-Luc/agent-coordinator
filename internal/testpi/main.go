package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type capture struct {
	PID      int               `json:"pid"`
	Args     []string          `json:"args"`
	Body     string            `json:"body"`
	Cwd      string            `json:"cwd"`
	Env      map[string]string `json:"env"`
	TaskPath string            `json:"task_path"`
}

func main() { os.Exit(run()) }

func run() int {
	var taskPath string
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "@") {
			taskPath = strings.TrimPrefix(arg, "@")
		}
	}
	body, _ := os.ReadFile(taskPath)
	cwd, _ := os.Getwd()
	nativePause := os.Getenv("TESTPI_NATIVE_PAUSE") == "1"
	var saved *unix.Termios
	if nativePause {
		if os.Getenv("TESTPI_PAUSED") == "" || os.Getenv("TESTPI_RESTORED") == "" {
			return 96
		}
		var err error
		saved, err = unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TIOCGETA)
		if err != nil {
			return 96
		}
		if err := setRawInput(*saved); err != nil {
			return 96
		}
		defer func() { _ = unix.IoctlSetTermios(int(os.Stdin.Fd()), unix.TIOCSETA, saved) }()
	}

	inputCapture := os.Getenv("TESTPI_INPUT_CAPTURE")
	inputErrors := make(chan error, 1)
	if inputCapture != "" {
		ready := make(chan error, 1)
		go recordInput(inputCapture, os.Getenv("TESTPI_INPUT_READY"), ready, inputErrors, nativePause, saved, os.Getenv("TESTPI_PAUSED"), os.Getenv("TESTPI_RESTORED"))
		if err := <-ready; err != nil {
			return 97
		}
	}

	env := make(map[string]string)
	for _, key := range []string{"AC_STATE_DIR", "AC_OWNER", "AC_JOB", "TESTPI_CAPTURE", "TESTPI_RELEASE", "PI_SESSION_ID", "PI_SESSION_FILE", "PI_PROVIDER", "PI_MODEL", "PI_REASONING_LEVEL", "PI_CODING_AGENT_DIR", "TEST_PROVIDER_KEY", "TESTPI_INPUT_CAPTURE", "TESTPI_INPUT_READY", "TESTPI_NATIVE_PAUSE", "TESTPI_PAUSED", "TESTPI_RESTORED"} {
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}
	record := capture{PID: os.Getpid(), Args: os.Args[1:], Body: string(body), Cwd: cwd, Env: env, TaskPath: taskPath}
	encoded, err := json.Marshal(record)
	if err != nil {
		return 90
	}
	file, err := os.OpenFile(os.Getenv("TESTPI_CAPTURE"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return 91
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		_ = file.Close()
		return 92
	}
	if err := file.Close(); err != nil {
		return 93
	}

	release := os.Getenv("TESTPI_RELEASE")
	if release != "" {
		for {
			if _, err := os.Stat(filepath.Clean(release)); err == nil {
				break
			}
			select {
			case err := <-inputErrors:
				if err != nil {
					return 98
				}
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	if status := os.Getenv("TESTPI_EXIT"); status != "" {
		if status == "ok" {
			return 0
		}
		if len(status) == 1 && status[0] >= '0' && status[0] <= '9' {
			return int(status[0] - '0')
		}
		return 94
	}
	return 0
}

func setRawInput(saved unix.Termios) error {
	raw := saved
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.ISIG
	raw.Iflag &^= unix.IXON | unix.ICRNL
	raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 1, 0
	return unix.IoctlSetTermios(int(os.Stdin.Fd()), unix.TIOCSETA, &raw)
}

func recordInput(path, readyPath string, ready chan<- error, failures chan<- error, nativePause bool, saved *unix.Termios, pausedPath, restoredPath string) {
	if readyPath != "" {
		if err := os.WriteFile(readyPath, nil, 0600); err != nil {
			ready <- err
			return
		}
	}
	ready <- nil
	buffer := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buffer)
		if n > 0 {
			start := 0
			for index, value := range buffer[:n] {
				if nativePause && value == 0x1a {
					if writeInput(path, buffer[start:index]) != nil {
						failures <- errors.New("input capture failed")
						return
					}
					if err := pauseNativeUI(*saved, pausedPath, restoredPath); err != nil {
						failures <- err
						return
					}
					start = index + 1
				}
			}
			if writeInput(path, buffer[start:n]) != nil {
				failures <- errors.New("input capture failed")
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func writeInput(path string, input []byte) error {
	if len(input) == 0 {
		return nil
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(input)
	closeErr := file.Close()
	return errors.Join(writeErr, closeErr)
}

func pauseNativeUI(saved unix.Termios, pausedPath, restoredPath string) error {
	continued := make(chan os.Signal, 1)
	signal.Notify(continued, syscall.SIGCONT)
	defer signal.Stop(continued)
	if err := unix.IoctlSetTermios(int(os.Stdin.Fd()), unix.TIOCSETA, &saved); err != nil {
		return err
	}
	if err := os.WriteFile(pausedPath, []byte("paused\n"), 0600); err != nil {
		return err
	}
	if err := syscall.Kill(0, syscall.SIGTSTP); err != nil {
		return err
	}
	<-continued
	if err := setRawInput(saved); err != nil {
		return err
	}
	return os.WriteFile(restoredPath, []byte("restored\n"), 0600)
}
