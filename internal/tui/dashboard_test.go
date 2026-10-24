package tui

import (
	"fmt"
	"strings"
	"testing"

	"agent-coordinator/internal/coordinator"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
)

func TestDashboardKeepsActionStatusInsideTheViewport(t *testing.T) {
	for _, size := range []struct{ width, height int }{{80, 24}, {100, 30}, {30, 12}} {
		t.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(t *testing.T) {
			m := newModel(t.Context(), nil)
			m.width, m.height = size.width, size.height
			m.jobs = []coordinator.JobFacts{{ID: "job", Name: "worker", Terminal: "live"}}
			m.previewText = strings.Repeat("live output\n", 40)
			m.loading = true
			for _, message := range []string{"Returned from attachment; refreshing session facts.", "Enter and Observe require an ordinary terminal outside tmux"} {
				m.message = message
				view := ansi.Strip(m.View())
				prefix := "Returned from attachment"
				if strings.HasPrefix(message, "Enter") {
					prefix = "Enter and Observe"
				}
				if !strings.Contains(view, prefix) {
					t.Fatalf("action status clipped below viewport:\n%s", view)
				}
				if !strings.Contains(view, "q quit") {
					t.Fatalf("contextual controls clipped by status:\n%s", view)
				}
				if len(strings.Split(view, "\n")) > size.height {
					t.Fatal("status fix overflowed the viewport")
				}
			}
		})
	}
}

func TestDashboardDetailsKeepOutcomeAndRecoveryGuidance(t *testing.T) {
	for _, size := range []struct{ width, height int }{{80, 24}, {100, 30}, {72, 12}} {
		t.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(t *testing.T) {
			m := newModel(t.Context(), nil)
			m.width, m.height = size.width, size.height
			job := coordinator.JobFacts{ID: "123e4567-e89b-42d3-a456-426614174001", Name: "worker", Launch: "unknown", Terminal: "unknown", PiReadiness: "unverified", TaskOutcome: "unknown", NextAction: "legacy boot identity; native session may still be live"}
			m.jobs = []coordinator.JobFacts{job}
			updated, _ := m.Update(resultMsg{operation: inspectJob, job: job})
			m = updated.(model)
			view := ansi.Strip(m.View())
			text := strings.Join(strings.Fields(view), " ")
			for _, required := range []string{"outcome=unknown", "native session may still be live", "q quit"} {
				if !strings.Contains(text, required) {
					t.Fatalf("details lost %q at ordinary size:\n%s", required, view)
				}
			}
			if len(strings.Split(view, "\n")) > size.height {
				t.Fatal("wrapped details overflowed viewport")
			}
		})
	}
}

func TestDashboardShowsSharedClientCount(t *testing.T) {
	m := newModel(t.Context(), nil)
	m.jobs = []coordinator.JobFacts{{ID: "job", Name: "worker", Clients: 3, Terminal: "live"}}
	view := ansi.Strip(m.View())
	if !strings.Contains(view, "3 clients") || !strings.Contains(view, "share input") {
		t.Fatalf("shared writable clients were hidden:\n%s", view)
	}
}

func TestObserveStartsLiveWithoutAnExtraToggle(t *testing.T) {
	if !newModel(t.Context(), nil).observe {
		t.Fatal("dashboard must start with live Observe enabled")
	}
}

func TestSelectionChangeRetainsTheInFlightPreviewLease(t *testing.T) {
	m := model{jobs: []coordinator.JobFacts{{ID: "a", Name: "first"}, {ID: "b", Name: "second"}}, previewLoading: true, previewJobID: "a", previewGen: 2, viewGeneration: 2}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	got := updated.(model)
	if !got.previewLoading || got.previewJobID != "a" {
		t.Fatal("selection change released a capture that is still running")
	}
	updated, _ = got.Update(previewMsg{jobID: "a", generation: 2, text: "old output"})
	got = updated.(model)
	if got.previewLoading || got.previewText != "" {
		t.Fatal("completed old capture must release its lease without replacing the new selection")
	}
}

func TestLateControlResolutionCannotInterruptNewTask(t *testing.T) {
	job := coordinator.JobFacts{ID: "job", Name: "worker", Session: &coordinator.SessionStatus{SessionID: "native", Generation: "runtime"}}
	m := model{coordinator: &coordinator.Coordinator{}, jobs: []coordinator.JobFacts{job}}
	pending, _ := m.resolveControl(stopAction)
	requestGeneration := pending.viewGeneration
	updated, _ := pending.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	updated, _ = updated.(model).Update(targetMsg{jobID: "job", generation: requestGeneration, action: stopAction, target: coordinator.SessionTarget{ID: "job", SessionID: "native", Generation: "runtime"}})
	if got := updated.(model); got.mode != newTask || got.controlTarget != nil {
		t.Fatal("late stop resolution interrupted the newer task form")
	}
}

func TestNewestControlResolutionWins(t *testing.T) {
	job := coordinator.JobFacts{ID: "job", Name: "worker", Session: &coordinator.SessionStatus{SessionID: "native", Generation: "runtime"}}
	m := model{coordinator: &coordinator.Coordinator{}, jobs: []coordinator.JobFacts{job}}
	first, _ := m.resolveControl(sendAction)
	firstGeneration := first.viewGeneration
	latest, _ := first.resolveControl(stopAction)
	updated, _ := latest.Update(targetMsg{jobID: "job", generation: firstGeneration, action: sendAction, target: coordinator.SessionTarget{ID: "job", SessionID: "native", Generation: "runtime"}})
	if got := updated.(model); got.controlTarget != nil || got.mode != dashboard {
		t.Fatal("older resolution overrode the newer control intent")
	}
}

func TestPreviewSanitizesControlsAndClipsByTerminalCells(t *testing.T) {
	got := renderPreview("safe\x1b]52;c;payload\a\x00 世界", 9)
	if strings.ContainsAny(got, "\x1b\a\x00") {
		t.Fatalf("preview contains executable controls: %q", got)
	}
	if strings.Contains(got, "payload") || runewidth.StringWidth(got) > 9 {
		t.Fatalf("preview was not clipped: %q", got)
	}
}

func TestRefreshFiltersOnlyConfirmedExitedAndKeepsSelectionByUUID(t *testing.T) {
	m := model{jobs: []coordinator.JobFacts{
		{ID: "a", Name: "active", Launch: "live"},
		{ID: "gone", Name: "gone", Launch: "exited"},
		{ID: "unknown", Name: "unknown", Launch: "unknown"},
	}, selected: 1}
	updated, _ := m.Update(resultMsg{operation: loadJobs, jobs: []coordinator.JobFacts{
		{ID: "a", Name: "active", Launch: "live"},
		{ID: "gone", Name: "gone", Launch: "exited"},
		{ID: "unknown", Name: "unknown", Launch: "unknown"},
	}})
	got := updated.(model)
	if len(got.jobs) != 2 || got.jobs[1].ID != "unknown" {
		t.Fatalf("refresh retained exited row or lost unknown row: %+v", got.jobs)
	}
	if got.selected != 0 || got.jobs[got.selected].ID != "a" {
		t.Fatalf("removed selected row retargeted neighbor: selected=%d jobs=%+v", got.selected, got.jobs)
	}
}

func TestObserveDistinguishesNativePromptAgentReportAndUnknown(t *testing.T) {
	attention := "needs review\x1b"
	job := coordinator.JobFacts{ID: "job", Name: "worker", Session: &coordinator.SessionStatus{Activity: "idle"}}
	idle := renderObserve(job, 80, 12, false, "", "")
	if strings.Contains(idle, "Needs your input") || !strings.Contains(idle, "No observed input request") {
		t.Fatalf("idle activity was misreported as input-needed:\\n%s", idle)
	}
	job.Session.Prompt = &coordinator.ObservedPrompt{Kind: "confirm", Title: "Approve?"}
	prompt := renderObserve(job, 80, 12, false, "", "")
	if !strings.Contains(prompt, "Needs your input") || !strings.Contains(prompt, "Approve?") {
		t.Fatalf("native prompt was not shown:\\n%s", prompt)
	}
	job.Session.Report = &coordinator.AgentReport{Summary: "working", Attention: &attention}
	both := renderObserve(job, 80, 12, false, "", "")
	if !strings.Contains(both, "Needs your input") || !strings.Contains(both, "Agent reports attention") {
		t.Fatalf("native prompt and agent attention were not both shown:\n%s", both)
	}
	narrowBoth := renderNarrowObserve(job, 80, 4, "output", "")
	if !strings.Contains(narrowBoth, "Needs input") || !strings.Contains(narrowBoth, "Agent attention") {
		t.Fatalf("narrow layout hid simultaneous attention:\n%s", narrowBoth)
	}
	job.Session.Prompt = nil
	report := renderObserve(job, 80, 12, false, "", "")
	if !strings.Contains(report, "Agent reports attention") || strings.ContainsAny(report, "\x1b\a\x00") {
		t.Fatalf("agent report was conflated or not sanitized:\\n%s", report)
	}
	job.Session = nil
	unknown := renderObserve(job, 80, 12, false, "", "")
	if !strings.Contains(unknown, "Attention unknown") || strings.Contains(unknown, "No observed input request") {
		t.Fatalf("missing observation was presented as all-clear:\\n%s", unknown)
	}
}

func TestUnknownObserverActivityIsNotReportedAsAllClear(t *testing.T) {
	job := coordinator.JobFacts{ID: "job", Name: "worker", Session: &coordinator.SessionStatus{Activity: "unknown"}}
	view := renderObserve(job, 80, 12, false, "", "")
	if !strings.Contains(view, "Attention unknown") || strings.Contains(view, "No observed input request") {
		t.Fatalf("unknown observer activity was presented as clear:\n%s", view)
	}
}

func TestStalePreviewDoesNotClearNewerPreviewFlight(t *testing.T) {
	m := model{jobs: []coordinator.JobFacts{{ID: "a", Name: "first"}, {ID: "b", Name: "second"}}, selected: 1, previewLoading: true, previewJobID: "b", previewGen: 3, viewGeneration: 3}
	updated, _ := m.Update(previewMsg{jobID: "a", generation: 2, text: "stale output"})
	if !updated.(model).previewLoading {
		t.Fatal("stale preview unlocked the newer in-flight capture")
	}
}

func TestRefreshResultDoesNotUnlockUnrelatedOperation(t *testing.T) {
	m := model{busy: true, loading: true, generation: 4}
	updated, _ := m.Update(resultMsg{operation: loadJobs, generation: 4})
	got := updated.(model)
	if !got.busy || got.loading {
		t.Fatalf("refresh changed unrelated busy state: busy=%v loading=%v", got.busy, got.loading)
	}
}

func TestPreviewResultCannotOverwriteNewerSelection(t *testing.T) {
	m := model{jobs: []coordinator.JobFacts{{ID: "a", Name: "first"}, {ID: "b", Name: "second"}}, selected: 1, previewLoading: true, previewJobID: "b", previewGen: 3, viewGeneration: 3}
	updated, _ := m.Update(previewMsg{jobID: "a", generation: 2, text: "stale output"})
	got := updated.(model)
	if got.previewText != "" || got.previewError != "" {
		t.Fatalf("stale preview replaced selected session output: %+v", got)
	}
	updated, _ = got.Update(previewMsg{jobID: "b", generation: 3, text: "current output"})
	if text := updated.(model).previewText; text != "current output" {
		t.Fatalf("current preview not accepted: %q", text)
	}
}

func TestDashboardOffersLiteralSteeringAndConfirmedStopControls(t *testing.T) {
	view := newModel(t.Context(), nil).View()
	for _, key := range []string{"m steer", "s stop", "X force-stop"} {
		if !strings.Contains(view, key) {
			t.Errorf("dashboard omits targeted control %q:\\n%s", key, view)
		}
	}
}

func TestRefreshFencesPendingTargetAcrossNativeSessionChange(t *testing.T) {
	m := model{jobs: []coordinator.JobFacts{{ID: "job", Name: "worker", Session: &coordinator.SessionStatus{SessionID: "native", Generation: "old"}}}, viewGeneration: 4}
	updated, _ := m.Update(resultMsg{operation: loadJobs, jobs: []coordinator.JobFacts{{ID: "job", Name: "worker", Session: &coordinator.SessionStatus{SessionID: "native", Generation: "new"}}}})
	got, _ := updated.(model).Update(targetMsg{jobID: "job", generation: 4, action: stopAction, target: coordinator.SessionTarget{ID: "job", SessionID: "native", Generation: "old"}})
	if got.(model).mode != dashboard {
		t.Fatal("late target response opened confirmation for replaced session")
	}
}

func TestTargetResponseCannotOpenControlForMissingOrChangedSnapshot(t *testing.T) {
	for _, session := range []*coordinator.SessionStatus{nil, {SessionID: "native", Generation: "new"}} {
		m := model{jobs: []coordinator.JobFacts{{ID: "job", Name: "worker", Session: session}}}
		updated, _ := m.Update(targetMsg{jobID: "job", action: stopAction, target: coordinator.SessionTarget{ID: "job", SessionID: "native", Generation: "old"}})
		if updated.(model).mode != dashboard {
			t.Fatal("target response opened control for a missing or changed session snapshot")
		}
	}
}

func TestMissingSessionCannotMatchResolvedControlTarget(t *testing.T) {
	target := coordinator.SessionTarget{ID: "job", SessionID: "native", Generation: "runtime"}
	m := model{jobs: []coordinator.JobFacts{{ID: "job", Name: "worker"}}, controlTarget: &target}
	if m.controlTargetMatchesSelected() {
		t.Fatal("missing session observation was accepted for control")
	}
}

func TestRefreshCancelsControlBoundToRemovedSession(t *testing.T) {
	target := coordinator.SessionTarget{ID: "removed", SessionID: "native", Generation: "runtime"}
	m := model{mode: composeMessage, controlTarget: &target, controlText: "draft", jobs: []coordinator.JobFacts{{ID: "removed", Name: "old"}}, selected: 0}
	updated, _ := m.Update(resultMsg{operation: loadJobs, jobs: []coordinator.JobFacts{{ID: "neighbor", Name: "neighbor", Session: &coordinator.SessionStatus{SessionID: "other", Generation: "other"}}}})
	got := updated.(model)
	if got.mode != dashboard || got.controlTarget != nil || got.controlText != "" {
		t.Fatalf("removed target retained draft/control state: mode=%v target=%+v draft=%q", got.mode, got.controlTarget, got.controlText)
	}
}

func TestComposeKeepsPastedDirectionLiteral(t *testing.T) {
	m := model{mode: composeMessage}
	const text = "Approve — Build\\x1b\nsecond line"
	updated, _ := m.updateCompose(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text), Paste: true})
	if got := updated.(model).controlText; got != text {
		t.Fatalf("compose changed literal direction: %q", got)
	}
}

func TestConfirmActionCannotRetargetAfterSelectionChanges(t *testing.T) {
	m := model{jobs: []coordinator.JobFacts{{ID: "first", Name: "first"}, {ID: "second", Name: "second"}}, selected: 0}
	m.setConfirmation(forgetJob)
	updated, _ := m.Update(resultMsg{operation: loadJobs, jobs: []coordinator.JobFacts{{ID: "second", Name: "second"}}})
	updated, command := updated.(model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if command != nil || updated.(model).busy {
		t.Fatal("confirmation acted on a different selected row")
	}
}

func TestDashboardOutputStaysWithinTerminalBounds(t *testing.T) {
	m := newModel(t.Context(), nil)
	m.jobs = []coordinator.JobFacts{{ID: "long-id", Name: strings.Repeat("界", 300), PiReadiness: strings.Repeat("unknown ", 300), TaskOutcome: "unknown"}}
	m.message = strings.Repeat("diagnostic ", 300)
	for _, size := range []struct{ width, height int }{{100, 30}, {30, 12}, {72, 12}} {
		m.width, m.height = size.width, size.height
		view := m.View()
		if lipgloss.Width(view) > size.width || lipgloss.Height(view) > size.height {
			t.Errorf("%dx%d view exceeded bounds: %dx%d", size.width, size.height, lipgloss.Width(view), lipgloss.Height(view))
		}
	}
}

func TestSessionListKeepsSelectedRowInViewport(t *testing.T) {
	jobs := make([]coordinator.JobFacts, 30)
	for index := range jobs {
		jobs[index] = coordinator.JobFacts{ID: "id", Name: fmt.Sprintf("job-%02d", index)}
	}
	view := renderSessionList(jobs, 29, 40, 6)
	if !strings.Contains(view, "job-29") || strings.Contains(view, "job-00") {
		t.Fatalf("viewport did not follow selected row:\n%s", view)
	}
}

func TestNarrowDashboardKeepsPreviewVisible(t *testing.T) {
	m := newModel(t.Context(), nil)
	m.width, m.height, m.observe = 30, 12, true
	m.jobs = []coordinator.JobFacts{{ID: "id", Name: "worker"}}
	m.previewText = "ok"
	if view := m.View(); !strings.Contains(view, "Output · ok") {
		t.Fatalf("narrow layout clipped the visible preview:\n%s", view)
	}
}

func TestObserveToggleControlsOnlyPreviewVisibility(t *testing.T) {
	m := newModel(t.Context(), nil)
	m.jobs = []coordinator.JobFacts{{ID: "id", Name: "worker"}}
	m.previewText = "preview-only-marker"
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	m = updated.(model)
	hidden := m.View()
	if strings.Contains(hidden, "preview-only-marker") {
		t.Fatal("hidden preview was rendered")
	}
	m.width = 30
	if narrow := m.View(); strings.Contains(narrow, "preview-only-marker") {
		t.Fatal("narrow layout showed preview while Observe was hidden")
	}
	m.width = 100
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	m = updated.(model)
	visible := m.View()
	if !strings.Contains(visible, "preview-only-marker") {
		t.Fatal("visible preview was omitted")
	}
}

func TestDashboardPreviewLayoutIsObservable(t *testing.T) {
	m := newModel(t.Context(), nil)
	m.jobs = []coordinator.JobFacts{{ID: "id", Name: "worker"}}
	view := m.View()
	if !strings.Contains(view, "Sessions") || !strings.Contains(view, "Observe") {
		t.Fatalf("dashboard lacks session list and Observe pane:\n%s", view)
	}
	m.width = 30
	if narrow := m.View(); !strings.Contains(narrow, "72×12") || strings.Contains(narrow, "╭") {
		t.Fatalf("narrow view overflows instead of explaining minimum size:\n%s", narrow)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 30, Height: 12})
	if updated.(model).width != 30 {
		t.Fatal("window resize was not recorded")
	}
}
