package tui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"agent-coordinator/internal/coordinator"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
)

type mode int

const (
	dashboard mode = iota
	newTask
	confirmAction
	composeMessage
	confirmControl
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
	operation  operation
	jobs       []coordinator.JobFacts
	job        coordinator.JobFacts
	err        error
	generation uint64
}
type attachMsg struct {
	cmd *exec.Cmd
	err error
}
type attachFinishedMsg struct{ err error }
type refreshTickMsg time.Time
type refreshStartMsg struct{}
type previewTickMsg time.Time
type previewMsg struct {
	jobID      string
	generation uint64
	text       string
	err        error
}
type controlAction int

const (
	sendAction controlAction = iota
	stopAction
	forceStopAction
)

type targetMsg struct {
	jobID      string
	generation uint64
	action     controlAction
	target     coordinator.SessionTarget
	err        error
}
type controlResultMsg struct {
	message string
	err     error
}

type model struct {
	ctx            context.Context
	coordinator    *coordinator.Coordinator
	mode           mode
	jobs           []coordinator.JobFacts
	selected       int
	field          int
	name           string
	cwd            string
	task           string
	controlText    string
	controlTarget  *coordinator.SessionTarget
	controlForce   bool
	confirm        operation
	confirmJobID   string
	confirmSession string
	confirmGen     string
	message        string
	busy           bool
	width          int
	height         int
	loading        bool
	generation     uint64
	observe        bool
	previewText    string
	previewError   string
	previewLoading bool
	previewJobID   string
	previewGen     uint64
	viewGeneration uint64
}

func newModel(ctx context.Context, c *coordinator.Coordinator) model {
	return model{ctx: ctx, coordinator: c, width: 100, height: 30, observe: true}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(func() tea.Msg { return refreshStartMsg{} }, refreshTick(), previewTick())
}

func (m model) beginRefresh() (model, tea.Cmd) {
	if m.loading {
		return m, nil
	}
	m.loading = true
	m.generation++
	return m, m.loadGeneration(m.generation)
}

func (m model) loadGeneration(generation uint64) tea.Cmd {
	return func() tea.Msg {
		result, err := m.coordinator.List(m.ctx)
		return resultMsg{operation: loadJobs, jobs: result.Jobs, err: err, generation: generation}
	}
}

func previewTick() tea.Cmd {
	return tea.Tick(time.Second, func(now time.Time) tea.Msg { return previewTickMsg(now) })
}

func refreshTick() tea.Cmd {
	return tea.Tick(2*time.Second, func(now time.Time) tea.Msg { return refreshTickMsg(now) })
}

func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case refreshStartMsg:
		return m.beginRefresh()
	case refreshTickMsg:
		updated, load := m.beginRefresh()
		return updated, tea.Batch(load, refreshTick())
	case previewTickMsg:
		cmd := previewTick()
		job, ok := m.selectedJob()
		if !ok || !m.observe || m.coordinator == nil || m.previewLoading || job.ID == "" || job.Retired {
			return m, cmd
		}
		m.previewLoading = true
		id, generation := job.ID, m.viewGeneration
		m.previewJobID, m.previewGen = id, generation
		capture := func() tea.Msg {
			text, err := m.coordinator.Preview(m.ctx, id)
			return previewMsg{jobID: id, generation: generation, text: text, err: err}
		}
		return m, tea.Batch(capture, cmd)
	case previewMsg:
		if message.jobID != m.previewJobID || message.generation != m.previewGen {
			return m, nil
		}
		m.previewLoading = false
		m.previewJobID = ""
		job, ok := m.selectedJob()
		if !ok || message.jobID != job.ID || message.generation != m.viewGeneration {
			return m, nil
		}
		if message.err != nil {
			m.previewError = "Live capture unavailable"
			m.previewText = ""
		} else {
			m.previewError = ""
			m.previewText = message.text
		}
		return m, nil
	case targetMsg:
		job, ok := m.selectedJob()
		if !ok || message.jobID != job.ID || message.generation != m.viewGeneration {
			return m, nil
		}
		if message.err != nil {
			m.message = message.err.Error()
			return m, nil
		}
		if job.Session == nil || message.target.ID != job.ID || message.target.SessionID != job.Session.SessionID || message.target.Generation != job.Session.Generation {
			m.message = "Selected session changed; resolve it again."
			return m, nil
		}
		m.controlTarget = &message.target
		if message.action == sendAction {
			m.controlText = ""
			m.mode = composeMessage
		} else {
			m.controlForce = message.action == forceStopAction
			m.mode = confirmControl
		}
		return m, nil
	case controlResultMsg:
		m.busy = false
		m.mode = dashboard
		m.controlText = ""
		m.controlTarget = nil
		if message.err != nil {
			m.message = message.err.Error()
		} else {
			m.message = message.message
		}
		return m, nil
	case tea.WindowSizeMsg:
		m.width, m.height = message.Width, message.Height
		return m, nil
	case resultMsg:
		if message.operation == loadJobs {
			if message.generation != m.generation {
				return m, nil
			}
			m.loading = false
		}
		if message.operation != loadJobs {
			m.busy = false
		}
		if message.err != nil {
			m.message = message.err.Error()
			var action *coordinator.ActionError
			if message.operation == startJob && errors.As(message.err, &action) && action.Diagnostic.ID != "" {
				m.mode = dashboard
				return m.beginRefresh()
			}
		} else if message.operation == loadJobs {
			selectedID, selectedSessionID, selectedGeneration := "", "", ""
			selectedHadSession := false
			if selected, ok := m.selectedJob(); ok {
				selectedID = selected.ID
				if selected.Session != nil {
					selectedHadSession = true
					selectedSessionID, selectedGeneration = selected.Session.SessionID, selected.Session.Generation
				}
			}
			m.jobs = activeJobs(message.jobs)
			m.selected = indexOfJob(m.jobs, selectedID)
			if m.selected < 0 {
				m.selected = 0
			}
			if m.selected >= len(m.jobs) {
				m.selected = len(m.jobs) - 1
			}
			if m.selected < 0 {
				m.selected = 0
			}
			if selectedID != "" {
				current, ok := m.selectedJob()
				if !ok || current.ID != selectedID || (current.Session != nil) != selectedHadSession || current.Session != nil && (current.Session.SessionID != selectedSessionID || current.Session.Generation != selectedGeneration) {
					m.selectionChanged()
				}
			}
			if m.mode == confirmAction && !m.confirmMatchesSelected() {
				m.mode = dashboard
				m.message = "Selected session changed; pending action cancelled."
			}
			if (m.mode == composeMessage || m.mode == confirmControl) && m.controlTarget != nil && !m.controlTargetMatchesSelected() {
				m.mode, m.controlTarget, m.controlText = dashboard, nil, ""
				m.message = "Selected session changed; pending control cancelled."
			}
		} else if message.operation == inspectJob {
			m.message = fmt.Sprintf("%s · %s: launch=%s terminal=%s readiness=%s outcome=%s; %s", message.job.ID, message.job.Name, message.job.Launch, message.job.Terminal, message.job.PiReadiness, message.job.TaskOutcome, message.job.NextAction)
			return m.beginRefresh()
		} else {
			if message.operation == startJob {
				m.mode = dashboard
				m.message = fmt.Sprintf("Started %s (%s): readiness unverified; outcome unknown.", message.job.Name, message.job.ID)
			} else {
				m.message = "Action complete. Readiness and task outcome remain unverified."
			}
			return m.beginRefresh()
		}
	case attachFinishedMsg:
		m.busy = false
		if message.err != nil {
			m.message = "Attach ended with error: " + message.err.Error()
		} else {
			m.message = "Returned from attachment; refreshing session facts. Task outcome remains unknown."
		}
		return m.beginRefresh()
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
		if m.mode == composeMessage {
			return m.updateCompose(message)
		}
		if m.mode == confirmControl {
			if message.String() != "y" && message.String() != "Y" {
				m.mode, m.controlTarget = dashboard, nil
				m.message = "Control action cancelled."
				return m, nil
			}
			if !m.controlTargetMatchesSelected() {
				m.mode, m.controlTarget = dashboard, nil
				m.message = "Selected session changed; resolve and confirm again."
				return m, nil
			}
			m.busy = true
			return m, m.stopSelected()
		}
		if m.mode == confirmAction {
			if message.String() == "y" || message.String() == "Y" {
				m.mode = dashboard
				if !m.confirmMatchesSelected() {
					m.message = "Selected session changed; resolve and confirm again."
					return m, nil
				}
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
			m.viewGeneration++
			m.mode, m.field = newTask, 0
			m.name, m.cwd, m.task, m.message = "", "", "", ""
		case "up":
			if m.selected > 0 {
				m.selected--
				m.selectionChanged()
			}
		case "down":
			if m.selected+1 < len(m.jobs) {
				m.selected++
				m.selectionChanged()
			}
		case "o":
			m.observe = !m.observe
		case "m":
			return m.resolveControl(sendAction)
		case "s":
			return m.resolveControl(stopAction)
		case "X":
			return m.resolveControl(forceStopAction)
		case "enter":
			job, ok := m.selectedJob()
			if !ok || job.Retired || job.ID == "" || job.Name == "" {
				m.message = "Select an active job to attach."
				return m, nil
			}
			m.viewGeneration++
			m.busy = true
			return m, m.attach(false)
		case "d":
			if !m.canRun(inspectJob) {
				m.message = "Select an active job first."
				return m, nil
			}
			m.viewGeneration++
			m.busy = true
			return m, m.runSelected(inspectJob)
		case "c":
			m.setConfirmation(continueJob)
		case "r":
			m.setConfirmation(repairJob)
		case "f":
			m.setConfirmation(forgetJob)
		case "l":
			if !m.loading {
				return m.beginRefresh()
			}
		}
	}
	return m, nil
}

func (m *model) setConfirmation(op operation) {
	job, ok := m.selectedJob()
	if !ok {
		m.message = "Select a job first."
		return
	}
	m.viewGeneration++
	m.confirm, m.mode = op, confirmAction
	m.confirmJobID = job.ID
	m.confirmSession, m.confirmGen = "", ""
	if job.Session != nil {
		m.confirmSession, m.confirmGen = job.Session.SessionID, job.Session.Generation
	}
}

func (m model) confirmMatchesSelected() bool {
	job, ok := m.selectedJob()
	if !ok || job.ID != m.confirmJobID {
		return false
	}
	if m.confirmSession == "" || m.confirmGen == "" {
		return job.Session == nil
	}
	return job.Session != nil && job.Session.SessionID == m.confirmSession && job.Session.Generation == m.confirmGen
}

func (m model) resolveControl(action controlAction) (model, tea.Cmd) {
	job, ok := m.selectedJob()
	if !ok || job.ID == "" || job.Name == "" || job.Retired || m.coordinator == nil {
		m.message = "Select a verified active session for control."
		return m, nil
	}
	m.viewGeneration++
	id, generation := job.ID, m.viewGeneration
	return m, func() tea.Msg {
		target, err := m.coordinator.ControlTarget(m.ctx, id)
		return targetMsg{jobID: id, generation: generation, action: action, target: target, err: err}
	}
}

func (m model) controlTargetMatchesSelected() bool {
	job, ok := m.selectedJob()
	return ok && m.controlTarget != nil && job.Session != nil && job.ID == m.controlTarget.ID && job.Session.SessionID == m.controlTarget.SessionID && job.Session.Generation == m.controlTarget.Generation
}

func (m model) stopSelected() tea.Cmd {
	target, force := *m.controlTarget, m.controlForce
	return func() tea.Msg {
		authorization, err := m.coordinator.PrepareStop(m.ctx, target, force)
		if err != nil {
			return controlResultMsg{err: err}
		}
		receipt, err := m.coordinator.ConfirmStop(m.ctx, authorization)
		if err != nil {
			return controlResultMsg{err: err}
		}
		message := "Graceful stop: " + receipt.Result + "; task outcome remains unknown."
		if force {
			message = "Force-stop: " + receipt.Result + "; detached children may survive and writes may be incomplete."
		}
		return controlResultMsg{message: message}
	}
}

func (m model) updateCompose(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc":
		m.mode, m.controlTarget, m.controlText = dashboard, nil, ""
		return m, nil
	case "enter":
		if strings.TrimSpace(m.controlText) == "" || len(m.controlText) > coordinator.MaxControlTextBytes {
			m.message = "Direction must be nonblank and within the message size limit."
			return m, nil
		}
		if !m.controlTargetMatchesSelected() || m.coordinator == nil {
			m.message = "Selected session changed; resolve it again."
			m.mode, m.controlTarget = dashboard, nil
			return m, nil
		}
		requestID, err := controlRequestID()
		if err != nil {
			m.message = "Could not create a unique request ID."
			return m, nil
		}
		target, text := *m.controlTarget, m.controlText
		m.busy = true
		return m, func() tea.Msg {
			receipt, err := m.coordinator.Send(m.ctx, target, requestID, text)
			if err != nil {
				return controlResultMsg{err: err}
			}
			return controlResultMsg{message: "Direction API invoked (" + receipt.Result + "); delivery and completion are unverified."}
		}
	case "backspace":
		if len(m.controlText) > 0 {
			_, size := utf8.DecodeLastRuneInString(m.controlText)
			m.controlText = m.controlText[:len(m.controlText)-size]
		}
	case "ctrl+j":
		m.controlText += "\n"
	default:
		if key.Type == tea.KeySpace {
			m.controlText += " "
		} else if key.Type == tea.KeyRunes {
			m.controlText += string(key.Runes)
		}
	}
	return m, nil
}

func controlRequestID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	encoded := hex.EncodeToString(raw[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
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
	return renderLiteralLine(job.Name) + " (" + renderLiteralLine(job.ID) + ")"
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

func (m *model) selectionChanged() {
	m.viewGeneration++
	m.previewText = ""
	m.previewError = ""
}

func activeJobs(jobs []coordinator.JobFacts) []coordinator.JobFacts {
	active := make([]coordinator.JobFacts, 0, len(jobs))
	for _, job := range jobs {
		if job.Launch != "exited" {
			active = append(active, job)
		}
	}
	return active
}

func indexOfJob(jobs []coordinator.JobFacts, id string) int {
	if id == "" {
		return -1
	}
	for index, job := range jobs {
		if job.ID == id {
			return index
		}
	}
	return -1
}

func (m model) selectedJobValue() coordinator.JobFacts {
	job, _ := m.selectedJob()
	return job
}

func (m model) View() string {
	var view strings.Builder
	narrow := m.width < 72 || m.height < 12
	attentionCount, unknownCount := 0, 0
	for _, job := range m.jobs {
		if sessionAttention(job) {
			attentionCount++
		}
		if job.Session == nil || job.Session.Activity == "unknown" {
			unknownCount++
		}
	}
	if narrow {
		view.WriteString("Agent coordinator\n")
	} else {
		view.WriteString(lipgloss.NewStyle().Bold(true).Render("Agent coordinator") + "\n")
		fmt.Fprintf(&view, "%d sessions · %d with known attention · %d observation unknown\n", len(m.jobs), attentionCount, unknownCount)
	}
	footer := m.renderFooter(narrow, attentionCount)
	footerHeight := 0
	if footer != "" {
		footer = clipView(footer, m.width, max(m.height-1, 1))
		footerHeight = len(strings.Split(footer, "\n"))
	}
	bodyHeight := max(m.height-footerHeight, 0)
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
	} else if m.mode == composeMessage {
		fmt.Fprintf(&view, "Direction to %s · %s · Enter sends literally; Ctrl+J adds a line; Esc cancels\n\n%s", m.selectedLabel(), m.targetGeneration(), renderLiteralText(m.controlText))
	} else if m.mode == confirmControl {
		if m.controlForce {
			fmt.Fprintf(&view, "Force-stop %s · %s? [y/N]\nSIGKILL may interrupt writes; detached children may survive.\n", m.selectedLabel(), m.targetGeneration())
		} else {
			fmt.Fprintf(&view, "Request graceful stop for %s · %s? [y/N]\nThe session may defer shutdown; no automatic escalation.\n", m.selectedLabel(), m.targetGeneration())
		}
	} else {
		if m.width >= 96 && m.height >= 24 {
			leftWidth := min(max(m.width*38/100, 32), 48)
			rightWidth := m.width - leftWidth - 1
			panelHeight := max(bodyHeight-4, 1)
			leftPanel := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("6")).Padding(0, 1).Width(leftWidth - 2).Height(panelHeight).Render(renderSessionList(m.jobs, m.selected, leftWidth-4, panelHeight))
			rightPanel := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.AdaptiveColor{Light: "8", Dark: "7"}).Padding(0, 1).Width(rightWidth - 2).Height(panelHeight).Render(renderObserve(m.selectedJobValue(), rightWidth-4, panelHeight, m.observe, m.previewText, m.previewError))
			view.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, leftPanel, " ", rightPanel))
		} else if narrow {
			available := max(bodyHeight-2, 0)
			previewHeight := min(4, max(available-2, 0))
			listHeight := available - previewHeight
			if listHeight > 0 {
				view.WriteString(renderSessionCards(m.jobs, m.selected, m.width, listHeight, 1) + "\n")
			}
			fmt.Fprintf(&view, "Min 72×12 · Attention %d\n", attentionCount)
			if previewHeight > 0 {
				view.WriteString(renderObserve(m.selectedJobValue(), m.width, previewHeight, m.observe, m.previewText, m.previewError))
			}
		} else {
			available := max(bodyHeight-2, 0)
			listHeight := min(7, max(available-8, 1))
			if !m.observe {
				listHeight = min(9, max(available-5, 1))
			}
			view.WriteString(renderSessionCards(m.jobs, m.selected, m.width, listHeight, 2) + "\n")
			if m.observe {
				view.WriteString(renderObserve(m.selectedJobValue(), m.width, available-listHeight, true, m.previewText, m.previewError))
			} else {
				view.WriteString(renderObserve(m.selectedJobValue(), m.width, available-listHeight, false, "", ""))
			}
		}
	}
	if bodyHeight == 0 {
		return clipView(footer, m.width, m.height)
	}
	body := clipView(view.String(), m.width, bodyHeight)
	if footer == "" {
		return body
	}
	return body + "\n" + footer
}

func (m model) renderFooter(narrow bool, attentionCount int) string {
	var lines []string
	if m.message != "" {
		lines = append(lines, clipView(renderLiteralText(m.message), m.width, 3))
	}
	if m.busy {
		lines = append(lines, "Working…")
	} else if m.loading && m.message == "" {
		lines = append(lines, "Refreshing session facts…")
	}
	if m.mode == dashboard && !m.busy {
		if narrow {
			lines = append(lines, "↑↓/o/Enter · n new · m send", "s stop · X force · d info", "c/r/f · l refresh · q quit")
		} else {
			lines = append(lines, "↑↓ select · Enter open · o Observe · n new · m steer", "s stop · X force-stop · d details · c Continue · r repair · f Forget", fmt.Sprintf("l refresh · q quit · %d clients share input · Shared cwd · F12 Return", m.selectedJobValue().Clients))
			if m.height >= 24 {
				lines = append(lines, "Pi readiness and task outcome are never inferred")
			}
		}
	}
	return strings.Join(lines, "\n")
}

func clipView(view string, width, height int) string {
	width, height = max(width, 1), max(height, 1)
	lines := make([]string, 0)
	for _, line := range strings.Split(view, "\n") {
		if lipgloss.Width(line) > width {
			lines = append(lines, strings.Split(ansi.Wrap(line, width, " "), "\n")...)
		} else {
			lines = append(lines, line)
		}
		if len(lines) >= height {
			break
		}
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

func (m model) targetGeneration() string {
	if m.controlTarget == nil {
		return ""
	}
	return renderLiteralLine(m.controlTarget.SessionID) + " · " + renderLiteralLine(m.controlTarget.Generation)
}

func lifecycle(job coordinator.JobFacts) string {
	if job.Launch == "exited" {
		return "Exited"
	}
	if job.Launch == "unknown" || job.Launch == "unreachable" {
		return "Status unknown"
	}
	if job.Terminal == "live" {
		return "Live"
	}
	if job.Launch == "" {
		return "Status unknown"
	}
	return renderLiteralLine(strings.ReplaceAll(job.Launch, "-", " "))
}

func renderSessionList(jobs []coordinator.JobFacts, selected, width, height int) string {
	return renderSessionCards(jobs, selected, width, height, 3)
}

func renderSessionCards(jobs []coordinator.JobFacts, selected, width, height, cardLines int) string {
	width, height = max(width, 1), max(height, 1)
	cardLines = min(cardLines, max(height-1, 1))
	cards := max((height-1)/cardLines, 1)
	start := 0
	if selected >= cards {
		start = selected - cards + 1
	}
	if start > len(jobs)-cards {
		start = max(len(jobs)-cards, 0)
	}
	end := min(start+cards, len(jobs))
	lines := []string{fmt.Sprintf("Sessions · Showing %d–%d of %d", min(start+1, len(jobs)), end, len(jobs))}
	if len(jobs) == 0 {
		return ansi.Truncate("No active rows · n new · l refresh", width, "…")
	}
	if height == 1 {
		lines = nil
	}
	for index := start; index < end; index++ {
		job := jobs[index]
		marker := "  "
		if index == selected {
			marker = "> "
		}
		name := renderLiteralLine(job.Name)
		attention := ""
		if job.Session == nil || job.Session.Activity == "unknown" {
			attention += "[?] "
		}
		if job.Session != nil && job.Session.Prompt != nil {
			attention += "[INPUT] "
		}
		if job.Session != nil && job.Session.Report != nil && job.Session.Report.Attention != nil && *job.Session.Report.Attention != "" {
			attention += "[AGENT] "
		}
		state := "Process " + lifecycle(job)
		if job.Retired {
			state = "retired"
		}
		name = ansi.Truncate(name, max(width-ansi.StringWidth(state)-4, 1), "…")
		label := marker + name + strings.Repeat(" ", max(width-ansi.StringWidth(marker+name+state), 1)) + state
		if index == selected {
			label = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true).Render(label)
		}
		lines = append(lines, label)
		if cardLines >= 2 {
			facts := strings.TrimSpace(attention)
			if facts != "" {
				facts += " · "
			}
			facts += "Activity " + activity(job)
			if attention == "" {
				facts += " · other coverage unknown"
			}
			facts = "  " + facts
			if sessionAttention(job) {
				facts = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Render(facts)
			}
			lines = append(lines, facts)
		}
		if cardLines == 3 {
			report := "No agent report"
			if job.Session != nil && job.Session.Report != nil && job.Session.Report.Summary != "" {
				report = "Agent: " + firstReportLine(job.Session.Report.Summary)
			}
			lines = append(lines, "  "+report)
		}
	}
	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], width, "…")
	}
	return strings.Join(lines, "\n")
}

func firstReportLine(summary string) string {
	for _, line := range strings.Split(summary, "\n") {
		if strings.TrimSpace(line) != "" {
			return renderLiteralLine(line)
		}
	}
	return ""
}

func activity(job coordinator.JobFacts) string {
	if job.Session == nil {
		return "unknown"
	}
	return renderLiteralLine(job.Session.Activity)
}

func sessionAttention(job coordinator.JobFacts) bool {
	return job.Session != nil && (job.Session.Prompt != nil || job.Session.Report != nil && job.Session.Report.Attention != nil && *job.Session.Report.Attention != "")
}

func renderObserve(job coordinator.JobFacts, width, height int, visible bool, preview, previewError string) string {
	width, height = max(width, 1), max(height, 1)
	title := "Observe · READ ONLY"
	if job.ID == "" && job.Name == "" {
		return clipView(title+"\nSelect a session · n new", width, height)
	}
	id := ansi.Truncate(renderLiteralLine(job.ID), 8, "")
	identity := renderLiteralLine(job.Name) + " · " + id
	lines := []string{title, identity}
	if height < 10 {
		lines = []string{title + " · " + identity}
	}
	var attention []string
	coverage := "Other prompt coverage unknown"
	if job.Session != nil {
		session := job.Session
		if session.Prompt != nil {
			attention = append(attention, "[INPUT] Needs your input · native "+renderLiteralLine(session.Prompt.Kind)+" · "+renderLiteralLine(session.Prompt.Title))
		}
		if session.Report != nil && session.Report.Attention != nil && *session.Report.Attention != "" {
			attention = append(attention, "[AGENT] Agent requests: "+renderLiteralLine(*session.Report.Attention))
		}
		if session.Prompt == nil && !sessionAttention(job) {
			coverage = "No observed input request · other coverage unknown"
		}
	}
	if job.Session == nil || job.Session.Activity == "unknown" {
		coverage = "[?] Attention unknown · Enter inspect"
	}
	outputRows := 1
	if visible {
		outputRows = 2
	}
	if len(lines)+len(attention)+1+outputRows < height {
		lines = append(lines, "Process "+lifecycle(job)+" · Activity "+activity(job))
	}
	lines = append(lines, attention...)
	lines = append(lines, coverage)
	if job.Session != nil && job.Session.Report != nil && job.Session.Report.Summary != "" && len(lines)+outputRows < height {
		lines = append(lines, "Agent report: "+firstReportLine(job.Session.Report.Summary))
	}
	if visible {
		if len(lines)+2 <= height {
			lines = append(lines, "Live output · current screen")
		}
		if len(lines) < height {
			switch {
			case previewError != "":
				lines = append(lines, renderLiteralLine(previewError)+" · Enter inspect")
			case preview == "":
				lines = append(lines, "Waiting for first capture…")
			default:
				output := strings.Split(renderPreview(preview, width), "\n")
				if height <= 4 {
					output[0] = "Output · " + output[0]
				}
				lines = append(lines, output...)
			}
		}
	} else {
		lines = append(lines, "Output hidden · o show")
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	for index, line := range lines {
		if (strings.HasPrefix(line, "[INPUT]") || strings.HasPrefix(line, "[AGENT]")) && ansi.StringWidth(line) > width && width >= 24 {
			line = ansi.Truncate(line, width-14, "…") + " Enter inspect"
		}
		line = ansi.Truncate(line, width, "…")
		if index == 0 {
			line = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true).Render(line)
		} else if strings.HasPrefix(line, "[INPUT]") || strings.HasPrefix(line, "[AGENT]") {
			line = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Render(line)
		}
		lines[index] = line
	}
	return strings.Join(lines, "\n")
}

func renderPreview(output string, width int) string {
	if width < 1 {
		return ""
	}
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	for index, line := range lines {
		lines[index] = runewidth.Truncate(renderLiteralLine(line), width, "…")
	}
	return strings.Join(lines, "\n")
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
