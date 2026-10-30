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

	"coordinate/internal/coordinator"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type mode int

const (
	dashboard mode = iota
	newTask
	confirmAction
	composeMessage
	confirmControl
	help
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
	inbox      *coordinator.CommunicationDiagnostics
	inboxErr   error
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
	communication  coordinator.CommunicationDiagnostics
	inboxError     bool
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
		inbox, inboxErr := m.coordinator.CommunicationMessages(m.ctx, 0)
		return resultMsg{operation: loadJobs, jobs: result.Jobs, err: err, generation: generation, inbox: &inbox, inboxErr: inboxErr}
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
			m.inboxError = message.inboxErr != nil
			if message.inbox != nil && message.inboxErr == nil {
				m.communication = *message.inbox
			}
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
			m.message = fmt.Sprintf("outcome=%s; %s · %s · %s: launch=%s terminal=%s readiness=%s", message.job.TaskOutcome, message.job.NextAction, message.job.Name, message.job.ID, message.job.Launch, message.job.Terminal, message.job.PiReadiness)
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
		if m.mode == help {
			switch message.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "esc", "h", "q":
				m.mode = dashboard
			}
			return m, nil
		}
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
		case "h":
			m.viewGeneration++
			m.mode = help
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
	if m.mode == help {
		m.mode = dashboard
		return overlayHelp(m.View(), m.width, m.height)
	}
	if m.mode == dashboard && m.width >= 72 && m.height >= 16 {
		return m.renderTableDashboard()
	}
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
		view.WriteString("Coordinate\n")
	} else {
		view.WriteString(lipgloss.NewStyle().Bold(true).Render("Coordinate") + "\n")
		if m.mode == dashboard {
			fmt.Fprintf(&view, "%d sessions · %d with known attention · %d observation unknown\n", len(m.jobs), attentionCount, unknownCount)
		}
	}
	footer := m.renderFooter(narrow)
	footerHeight := 0
	if footer != "" {
		footer = clipView(footer, m.width, max(m.height-1, 1))
		footerHeight = len(strings.Split(footer, "\n"))
	}
	bodyHeight := max(m.height-footerHeight, 0)
	if m.mode != dashboard {
		view.WriteString(m.renderModal(max(bodyHeight-1, 1)))
	} else {
		if narrow {
			available := max(bodyHeight-2, 0)
			previewHeight := min(4, max(available-1, 0))
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
			view.WriteString(renderObserve(m.selectedJobValue(), m.width, available-listHeight, m.observe, m.previewText, m.previewError))
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

// Kanagawa Ink roles, borrowed from Pi without coupling the prototype to its theme loader.
const (
	inkText         = lipgloss.Color("#8ea49e")
	inkPanel        = lipgloss.Color("#1F1F28")
	inkSelected     = lipgloss.Color("#363646")
	inkSelectedText = lipgloss.Color("#DCD7BA")
	inkAccent       = lipgloss.Color("#8992a7")
	inkBorder       = lipgloss.Color("#625e5a")
)

var inkWarning = lipgloss.CompleteColor{TrueColor: "#c4b28a", ANSI256: "180", ANSI: "3"}

func renderSectionBar(text string, width int) string {
	return lipgloss.NewStyle().Bold(true).Foreground(inkAccent).Background(inkPanel).Width(width).Render(ansi.Truncate(text, width, "…"))
}

func (m model) renderTableDashboard() string {
	known, unknown := 0, 0
	for _, job := range m.jobs {
		if sessionAttention(job) {
			known++
		}
		if job.Session == nil || job.Session.Activity == "unknown" {
			unknown++
		}
	}
	footerLines := strings.Split(m.renderFooter(false), "\n")
	if !m.busy && m.height >= 24 {
		footerLines[len(footerLines)-1] = renderSectionBar(" "+footerLines[len(footerLines)-1], m.width)
	}
	footer := clipView(strings.Join(footerLines, "\n"), m.width, m.height-1)
	bodyHeight := m.height - lipgloss.Height(footer)
	available := max(bodyHeight-1, 1)
	// shortcut: fit around the usual 120-column native screen; measure captures if variable widths become important.
	leftWidth := min(m.width/2-2, max(48, m.width-121))
	rightWidth := m.width - leftWidth - 1
	header := renderSectionBar(fmt.Sprintf(" Coordinate │ %d sessions │ attention %d │ unknown %d", len(m.jobs), known, unknown), m.width)
	table := renderSessionTable(m.jobs, m.selected, leftWidth, available)
	observe := renderObserve(m.selectedJobValue(), rightWidth, available, m.observe, m.previewText, m.previewError)
	left := lipgloss.NewStyle().Width(leftWidth).Height(available).Render(clipView(table, leftWidth, available))
	right := lipgloss.NewStyle().Width(rightWidth).Height(available).Render(observe)
	divider := lipgloss.NewStyle().Foreground(inkBorder).Render(strings.Repeat("│\n", available-1) + "│")
	panes := lipgloss.JoinHorizontal(lipgloss.Top, left, divider, right)
	body := clipView(header+"\n"+panes, m.width, bodyHeight)
	body += strings.Repeat("\n", max(bodyHeight-lipgloss.Height(body), 0))
	return lipgloss.NewStyle().Foreground(inkText).Render(body + "\n" + footer)
}

func renderSessionTable(jobs []coordinator.JobFacts, selected, width, height int) string {
	capacity := max(height-2, 1)
	start := min(max(selected-capacity+1, 0), max(len(jobs)-capacity, 0))
	end := min(start+capacity, len(jobs))
	nameWidth := max(width-30, 1)
	if width >= 72 {
		nameWidth = min(32, width/3)
	}
	cell := func(text string, width int) string {
		text = ansi.Truncate(text, width, "…")
		return text + strings.Repeat(" ", max(width-ansi.StringWidth(text), 0))
	}
	columns := func(name, process, activity, attention, report string) string {
		if width >= 72 {
			return cell(name, nameWidth) + " " + cell(process, 7) + " " + cell(activity, 8) + " " + cell(attention, 19) + " " + cell(report, width-nameWidth-40)
		}
		return cell(name, nameWidth) + " " + cell(process, 7) + " " + cell(attention, 19)
	}
	lines := []string{renderSectionBar(fmt.Sprintf(" Sessions · Showing %d–%d of %d", min(start+1, len(jobs)), end, len(jobs)), width)}
	labels := "  " + columns("Session", "State", "Activity", "Attention", "Report")
	lines = append(lines, lipgloss.NewStyle().Bold(true).Foreground(inkAccent).Render(ansi.Truncate(labels, width, "…")))
	if len(jobs) == 0 {
		lines = append(lines, "  No active rows · n new · l refresh")
	}
	for index := start; index < end; index++ {
		job := jobs[index]
		flags := ""
		if job.Session != nil && job.Session.Prompt != nil {
			flags += "[INPUT] "
		}
		if job.Session != nil && job.Session.Report != nil && job.Session.Report.Attention != nil && *job.Session.Report.Attention != "" {
			flags += "[AGENT] "
		}
		if job.Session == nil || job.Session.Activity == "unknown" {
			flags += "[?]"
		}
		flags = strings.TrimSpace(flags)
		if flags == "" {
			flags = "-"
		}
		process := lifecycle(job)
		if job.Retired {
			process = "retired"
		} else if process == "Status unknown" {
			process = "unknown"
		}
		report := "—"
		if job.Session != nil && job.Session.Report != nil && job.Session.Report.Summary != "" {
			report = firstReportLine(job.Session.Report.Summary)
		}
		row := "  " + columns(renderLiteralLine(job.Name), process, activity(job), flags, report)
		if index == selected {
			row = "> " + row[2:]
			row = lipgloss.NewStyle().Bold(true).Foreground(inkSelectedText).Background(inkSelected).Render(row)
		} else if sessionAttention(job) {
			row = "  " + columns(renderLiteralLine(job.Name), process, activity(job), lipgloss.NewStyle().Foreground(inkWarning).Render(flags), report)
		}
		lines = append(lines, ansi.Truncate(row, width, "…"))
	}
	return strings.Join(lines, "\n")
}

func (m model) renderModal(height int) string {
	width := max(m.width, 1)
	heading := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	if m.mode == newTask {
		lines := []string{heading.Render("New task")}
		fields := []struct{ label, text string }{
			{"Name", renderLiteralLine(m.name)},
			{"Working directory", renderLiteralLine(m.cwd)},
			{"Task", renderLiteralText(m.task)},
		}
		for index, field := range fields {
			marker := "  "
			if index == m.field {
				marker = "> "
			}
			prefix := marker + field.label + ": "
			rows := 1
			if index == 2 {
				rows = min(4, max(height-3, 1))
			}
			lines = append(lines, prefix+draftExcerpt(field.text, max(width-ansi.StringWidth(prefix), 1), rows))
		}
		if lipgloss.Height(strings.Join(lines, "\n")) < height {
			lines = append(lines, "Persistent Pi session · Shared cwd may cause conflicting edits.")
		}
		return clipView(strings.Join(lines, "\n"), width, height)
	}
	job := m.selectedJobValue()
	id, native, runtime := job.ID, "", ""
	if job.Session != nil {
		native, runtime = job.Session.SessionID, job.Session.Generation
	}
	if m.mode == confirmAction && m.confirmJobID != "" {
		id, native, runtime = m.confirmJobID, m.confirmSession, m.confirmGen
	} else if m.controlTarget != nil {
		id, native, runtime = m.controlTarget.ID, m.controlTarget.SessionID, m.controlTarget.Generation
	}
	identity := "Name: " + renderLiteralLine(job.Name) + "\nUUID: " + renderLiteralLine(id)
	identity += "\nNative session: " + renderLiteralLine(native) + "\nRuntime identity: " + renderLiteralLine(runtime)
	if m.mode == composeMessage {
		lines := heading.Render("Direction · send literally") + "\n" + identity + "\n> Direction:"
		lines = ansi.Wrap(lines, width, " ")
		if lipgloss.Height(lines)+1 > height {
			lines = heading.Render("Direction · send literally") + "\nResize to inspect full target.\n> Direction:"
		}
		rows := max(height-lipgloss.Height(lines), 1)
		return clipView(lines+"\n"+draftExcerpt(renderLiteralText(m.controlText), width, rows), width, height)
	}
	title, consequence := "Confirm "+operationName(m.confirm), "Explicit lifecycle action; does not establish task success."
	if m.mode == confirmControl {
		title, consequence = "Request graceful stop", "The session may defer shutdown; no automatic escalation."
		if m.controlForce {
			title = "Force-stop"
			consequence = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Render("SIGKILL may interrupt writes; detached children may survive.")
		}
	}
	content := heading.Render(title) + "\n" + identity + "\n" + consequence
	if lipgloss.Height(ansi.Wrap(content, width, " ")) > height {
		content = heading.Render(title) + "\nResize to inspect full target before confirming.\n" + consequence
	}
	return clipView(content, width, height)
}

func draftExcerpt(escaped string, width, height int) string {
	lines := strings.Split(ansi.Wrap(escaped, width, " "), "\n")
	if len(lines) > height {
		lines = lines[len(lines)-height:]
		for ansi.StringWidth(lines[0]) >= width {
			_, _, size, _ := ansi.DecodeSequence(lines[0], ansi.NormalState, nil)
			lines[0] = lines[0][size:]
		}
		lines[0] = "…" + lines[0]
	}
	return strings.Join(lines, "\n")
}

func (m model) renderFooter(narrow bool) string {
	var lines []string
	if m.communication.Pending > 0 || m.communication.Escalated > 0 {
		lines = append(lines, fmt.Sprintf("Outstanding %d · escalated %d", m.communication.Pending, m.communication.Escalated))
	}
	if m.inboxError {
		lines = append(lines, "Communication unavailable")
	} else if m.communication.Degraded {
		lines = append(lines, "Communication degraded")
	}
	if m.message != "" {
		lines = append(lines, "Action status", clipView(renderLiteralText(m.message), m.width, 3))
	}
	if m.busy {
		lines = append(lines, "Working…")
	}
	if !m.busy {
		switch m.mode {
		case newTask:
			lines = append(lines, "Tab field · Enter next/submit · Ctrl+J newline · Esc cancel")
		case composeMessage:
			lines = append(lines, "Enter send literally · Ctrl+J newline · Esc cancel")
		case confirmAction, confirmControl:
			lines = append(lines, "[y/N] · y confirm · any other key cancel")
		}
	}
	if m.mode == dashboard && !m.busy {
		lines = append(lines, "h help · q quit")
		if !narrow {
			lines = append(lines, fmt.Sprintf("%d clients share input · cwd shared · F12 return · readiness/outcome unverified", m.selectedJobValue().Clients))
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
		process := lifecycle(job)
		state := "Process " + process
		if job.Retired {
			process, state = "retired", "retired"
		}
		if cardLines == 1 {
			state = strings.TrimSpace(attention)
			if state == "" {
				state = process
			} else if width >= 2+min(ansi.StringWidth(name), 8)+1+ansi.StringWidth(state)+1+ansi.StringWidth(process) {
				state += " " + process
			}
		}
		name = ansi.Truncate(name, max(width-ansi.StringWidth(state)-4, 1), "…")
		nameStyle := lipgloss.NewStyle().Bold(true)
		if index == selected {
			nameStyle = nameStyle.Foreground(lipgloss.Color("6"))
		}
		label := marker + nameStyle.Render(name) + strings.Repeat(" ", max(width-ansi.StringWidth(marker+name+state), 1)) + state
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
	title := lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true).Render("Observe · READ ONLY")
	if width >= 32 && height >= 10 {
		title = renderSectionBar(" Observe · READ ONLY", width)
	}
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
			label := "[INPUT] Needs your input · native " + renderLiteralLine(session.Prompt.Kind)
			if session.Prompt.Title != "" {
				label += " · " + renderLiteralLine(session.Prompt.Title)
			}
			attention = append(attention, label)
		}
		if session.Report != nil && session.Report.Attention != nil && *session.Report.Attention != "" {
			attention = append(attention, "[AGENT] Agent requests: "+renderLiteralLine(*session.Report.Attention))
		}
		if !sessionAttention(job) {
			coverage = "No observed input request · other coverage unknown"
		}
	}
	if job.Session == nil || job.Session.Activity == "unknown" {
		coverage = "[?] Attention unknown · Enter inspect"
	}
	if len(lines)+len(attention)+1 > height {
		lines = nil
	}
	outputRows := 1
	if visible {
		outputRows = 2
	}
	if len(lines)+len(attention)+1+outputRows < height {
		lines = append(lines, "Process "+lifecycle(job)+" · Activity "+activity(job))
	}
	for index, line := range attention {
		suffix := " Enter inspect"
		if ansi.StringWidth(line) > width && width >= 24 {
			line = ansi.Truncate(line, width-ansi.StringWidth(suffix), "…") + suffix
		}
		attention[index] = lipgloss.NewStyle().Foreground(inkWarning).Render(line)
	}
	lines = append(lines, attention...)
	lines = append(lines, coverage)
	if job.Session != nil && job.Session.Report != nil && job.Session.Report.Summary != "" && len(lines)+outputRows < height {
		lines = append(lines, "Agent report: "+firstReportLine(job.Session.Report.Summary))
	}
	if visible {
		labelled := len(lines)+2 <= height
		if labelled {
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
				if !labelled {
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
	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], width, "…")
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
