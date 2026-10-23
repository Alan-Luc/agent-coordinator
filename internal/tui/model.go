package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"unicode/utf8"

	"agent-coordinator/internal/coordinator"
	tea "github.com/charmbracelet/bubbletea"
)

type mode int

const (
	dashboard mode = iota
	newTask
	confirmAction
)

type operation int

const (
	loadJobs operation = iota
	startJob
	inspectJob
	continueJob
	repairJob
	forgetJob
)

type resultMsg struct {
	operation operation
	jobs      []coordinator.JobFacts
	job       coordinator.JobFacts
	err       error
}
type attachMsg struct {
	cmd *exec.Cmd
	err error
}
type attachFinishedMsg struct{ err error }

type model struct {
	ctx         context.Context
	coordinator *coordinator.Coordinator
	mode        mode
	jobs        []coordinator.JobFacts
	selected    int
	field       int
	name        string
	cwd         string
	task        string
	confirm     operation
	message     string
	busy        bool
}

func newModel(ctx context.Context, c *coordinator.Coordinator) model {
	return model{ctx: ctx, coordinator: c}
}

func (m model) Init() tea.Cmd { return m.load() }

func (m model) load() tea.Cmd {
	return func() tea.Msg {
		result, err := m.coordinator.List(m.ctx)
		return resultMsg{operation: loadJobs, jobs: result.Jobs, err: err}
	}
}

func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case resultMsg:
		m.busy = false
		if message.err != nil {
			m.message = message.err.Error()
			var action *coordinator.ActionError
			if message.operation == startJob && errors.As(message.err, &action) && action.Diagnostic.ID != "" {
				m.mode = dashboard
				return m, m.load()
			}
		} else if message.operation == loadJobs {
			m.jobs = message.jobs
			if m.selected >= len(m.jobs) {
				m.selected = len(m.jobs) - 1
			}
			if m.selected < 0 {
				m.selected = 0
			}
		} else if message.operation == inspectJob {
			m.message = fmt.Sprintf("%s: launch=%s terminal=%s readiness=%s outcome=%s; %s", message.job.Name, message.job.Launch, message.job.Terminal, message.job.PiReadiness, message.job.TaskOutcome, message.job.NextAction)
			return m, m.load()
		} else {
			if message.operation == startJob {
				m.mode = dashboard
				m.message = fmt.Sprintf("Started %s (%s): readiness unverified; outcome unknown.", message.job.Name, message.job.ID)
			} else {
				m.message = "Action complete. Readiness and task outcome remain unverified."
			}
			return m, m.load()
		}
	case attachFinishedMsg:
		m.busy = false
		if message.err != nil {
			m.message = "Attach ended with error: " + message.err.Error()
		} else {
			m.message = "Returned from attachment; refreshing session facts. Task outcome remains unknown."
		}
		return m, m.load()
	case attachMsg:
		if message.err != nil {
			m.busy = false
			m.message = message.err.Error()
			return m, nil
		}
		m.busy = false
		return m, tea.ExecProcess(message.cmd, func(err error) tea.Msg { return attachFinishedMsg{err: err} })
	case tea.KeyMsg:
		if m.busy {
			return m, nil
		}
		if m.mode == newTask {
			return m.updateForm(message)
		}
		if m.mode == confirmAction {
			if message.String() == "y" || message.String() == "Y" {
				m.mode = dashboard
				if !m.canRun(m.confirm) {
					m.message = "This action is unavailable for the selected retired or missing job."
					return m, nil
				}
				m.busy = true
				return m, m.runSelected(m.confirm)
			}
			m.mode = dashboard
			m.message = "Action cancelled."
			return m, nil
		}
		switch message.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "n":
			m.mode, m.field = newTask, 0
			m.name, m.cwd, m.task, m.message = "", "", "", ""
		case "up":
			if m.selected > 0 {
				m.selected--
			}
		case "down":
			if m.selected+1 < len(m.jobs) {
				m.selected++
			}
		case "enter", "o":
			job, ok := m.selectedJob()
			if !ok || job.Retired || job.ID == "" || job.Name == "" {
				m.message = "Select an active job to attach."
				return m, nil
			}
			m.busy = true
			return m, m.attach(message.String() == "o")
		case "d":
			if !m.canRun(inspectJob) {
				m.message = "Select an active job first."
				return m, nil
			}
			m.busy = true
			return m, m.runSelected(inspectJob)
		case "c":
			m.confirm, m.mode = continueJob, confirmAction
		case "r":
			m.confirm, m.mode = repairJob, confirmAction
		case "f":
			m.confirm, m.mode = forgetJob, confirmAction
		case "l":
			return m, m.load()
		}
	}
	return m, nil
}

func (m model) updateForm(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if key.String() == "esc" {
		m.mode = dashboard
		return m, nil
	}
	if key.String() == "tab" {
		m.field = (m.field + 1) % 3
		return m, nil
	}
	if key.String() == "enter" {
		if m.field < 2 {
			m.field++
			return m, nil
		}
		m.busy = true
		name, cwd, task := m.name, m.cwd, []byte(m.task)
		return m, func() tea.Msg {
			job, err := m.coordinator.Start(m.ctx, name, cwd, task)
			return resultMsg{operation: startJob, job: job, err: err}
		}
	}
	var value *string
	switch m.field {
	case 0:
		value = &m.name
	case 1:
		value = &m.cwd
	default:
		value = &m.task
	}
	switch key.String() {
	case "backspace":
		if len(*value) > 0 {
			_, size := utf8.DecodeLastRuneInString(*value)
			*value = (*value)[:len(*value)-size]
		}
	case "ctrl+j":
		if m.field == 2 {
			*value += "\n"
		}
	default:
		if key.Type == tea.KeySpace {
			*value += " "
		} else if key.Type == tea.KeyRunes {
			*value += string(key.Runes)
		}
	}
	return m, nil
}

func (m model) selectedLabel() string {
	job, ok := m.selectedJob()
	if !ok {
		return "no job selected"
	}
	return renderLiteralLine(job.Name) + " (" + job.ID + ")"
}

func (m model) selectedJob() (coordinator.JobFacts, bool) {
	if m.selected < 0 || m.selected >= len(m.jobs) {
		return coordinator.JobFacts{}, false
	}
	return m.jobs[m.selected], true
}

func targetFor(job coordinator.JobFacts, op operation) (string, bool, bool) {
	if job.ID == "" || job.Name == "" {
		return "", false, false
	}
	if job.Retired && op != forgetJob {
		return "", false, false
	}
	return job.ID, true, true
}

func (m model) canRun(op operation) bool {
	job, ok := m.selectedJob()
	if !ok {
		return false
	}
	_, _, allowed := targetFor(job, op)
	return allowed
}

func (m model) runSelected(op operation) tea.Cmd {
	job, ok := m.selectedJob()
	if !ok {
		return nil
	}
	target, byID, allowed := targetFor(job, op)
	if !allowed {
		return nil
	}
	return func() tea.Msg {
		var err error
		switch op {
		case continueJob:
			err = m.coordinator.ContinueID(m.ctx, target)
		case repairJob:
			err = m.coordinator.RepairSocketID(m.ctx, target)
		case forgetJob:
			err = m.coordinator.Forget(m.ctx, target, byID)
		case inspectJob:
			job, e := m.coordinator.DoctorID(m.ctx, target)
			return resultMsg{operation: inspectJob, job: job, err: e}
		}
		return resultMsg{operation: op, err: err}
	}
}
func (m model) attach(observe bool) tea.Cmd {
	job, ok := m.selectedJob()
	if !ok || job.Retired || job.ID == "" || job.Name == "" {
		return nil
	}
	return func() tea.Msg {
		cmd, err := m.coordinator.AttachID(m.ctx, job.ID, observe)
		if err != nil {
			return attachMsg{err: err}
		}
		return attachMsg{cmd: cmd}
	}
}

func (m model) View() string {
	var view strings.Builder
	view.WriteString("Agent coordinator — Pi readiness and task outcome are never inferred\n\n")
	if m.mode == newTask {
		view.WriteString("New task (Tab changes field; Enter advances/submits; Ctrl+J adds a task line; Esc cancels)\n")
		fields := []struct{ label, text string }{
			{"Name", renderLiteralLine(m.name)},
			{"Working directory", renderLiteralLine(m.cwd)},
			{"Task", renderLiteralText(m.task)},
		}
		for index, field := range fields {
			marker := " "
			if index == m.field {
				marker = ">"
			}
			fmt.Fprintf(&view, "%s %s: %s\n", marker, field.label, field.text)
		}
		view.WriteString("\nEnter submits to a persistent Pi session. Shared cwd may cause conflicting edits.\n")
	} else if m.mode == confirmAction {
		fmt.Fprintf(&view, "Confirm %s for %s? [y/N]\n", operationName(m.confirm), m.selectedLabel())
		view.WriteString("This is an explicit lifecycle action; it does not establish task success.\n")
	} else {
		view.WriteString("↑/↓ select  n new  Enter writable  o observe  d doctor  c Continue  r repair socket  f Forget  l refresh  q quit\n\n")
		if len(m.jobs) == 0 {
			view.WriteString("No sessions found. Press n to start one.\n")
		}
		for i, job := range m.jobs {
			marker := " "
			if i == m.selected {
				marker = ">"
			}
			fmt.Fprintf(&view, "%s %-20s id=%s retired=%t launch=%-10s terminal=%-12s clients=%d readiness=%s outcome=%s\n", marker, job.Name, job.ID, job.Retired, job.Launch, job.Terminal, job.Clients, job.PiReadiness, job.TaskOutcome)
		}
		view.WriteString("\nShared cwd permits conflicting edits. Writable clients share input. Observe is read-only. F12 Return detaches only you.\n")
	}
	if m.message != "" {
		fmt.Fprintf(&view, "\n%s\n", renderLiteralText(m.message))
	}
	if m.busy {
		view.WriteString("\nWorking…\n")
	}
	return view.String()
}

func renderLiteralLine(line string) string {
	quoted := strconv.QuoteToGraphic(line)
	return quoted[1 : len(quoted)-1]
}

func renderLiteralText(value string) string {
	lines := strings.Split(value, "\n")
	for index, line := range lines {
		lines[index] = renderLiteralLine(line)
	}
	return strings.Join(lines, "\n")
}

func operationName(op operation) string {
	switch op {
	case continueJob:
		return "Continue signal"
	case repairJob:
		return "socket repair signal"
	case forgetJob:
		return "Forget"
	default:
		return "action"
	}
}

func Run(ctx context.Context, c *coordinator.Coordinator) error {
	if _, _, err := coordinator.TerminalSize(int(os.Stdin.Fd())); err != nil {
		return fmt.Errorf("dashboard requires terminal input and output: %w", err)
	}
	if _, _, err := coordinator.TerminalSize(int(os.Stdout.Fd())); err != nil {
		return fmt.Errorf("dashboard requires terminal input and output: %w", err)
	}
	program := tea.NewProgram(newModel(ctx, c), tea.WithContext(ctx))
	_, err := program.Run()
	return err
}
