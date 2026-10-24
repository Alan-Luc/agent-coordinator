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
	"github.com/muesli/termenv"
)

func TestDashboardWideShellShowsCardHierarchyAndTruthfulCounts(t *testing.T) {
	attention := "review requested"
	m := newModel(t.Context(), nil)
	m.width, m.height = 120, 30
	m.jobs = []coordinator.JobFacts{
		{ID: "one", Name: "planner", Launch: "live", Terminal: "live", Session: &coordinator.SessionStatus{Activity: "busy", Prompt: &coordinator.ObservedPrompt{Kind: "confirm"}, Report: &coordinator.AgentReport{Attention: &attention, Summary: "proposal ready"}}},
		{ID: "two", Name: "unknown", Launch: "live", Terminal: "live", Session: &coordinator.SessionStatus{Activity: "unknown"}},
	}
	m.selected = 0
	view := ansi.Strip(m.View())
	for _, required := range []string{"2 sessions", "1 with known attention", "1 observation unknown", "> planner", "Process Live", "[INPUT]", "[AGENT]", "Activity busy", "Agent: proposal ready", "Showing 1–2 of 2"} {
		if !strings.Contains(view, required) {
			t.Errorf("dashboard omitted %q:\n%s", required, view)
		}
	}
}

func TestDashboardUsesSplitOnlyAtApprovedThreshold(t *testing.T) {
	for _, size := range []struct {
		width, height int
		split         bool
	}{{120, 30, true}, {96, 24, true}, {95, 24, false}, {80, 24, false}} {
		t.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(t *testing.T) {
			m := newModel(t.Context(), nil)
			m.width, m.height = size.width, size.height
			m.jobs = []coordinator.JobFacts{{ID: "job", Name: "worker", Terminal: "live"}}
			view := m.View()
			if strings.Contains(view, "╭") != size.split {
				t.Fatalf("split=%v, want %v:\n%s", strings.Contains(view, "╭"), size.split, view)
			}
		})
	}
}

func TestSessionCardsPreserveStateAndSelectedViewport(t *testing.T) {
	jobs := make([]coordinator.JobFacts, 30)
	for index := range jobs {
		jobs[index] = coordinator.JobFacts{ID: fmt.Sprintf("id-%d", index), Name: fmt.Sprintf("job-%02d", index), Terminal: "live", Session: &coordinator.SessionStatus{Activity: "idle"}}
	}
	for _, selected := range []int{0, 1, 2, 14, 28, 29} {
		view := ansi.Strip(renderSessionList(jobs, selected, 40, 10))
		if !strings.Contains(view, fmt.Sprintf("> job-%02d", selected)) || strings.Count(view, "Process Live") != 3 || len(strings.Split(view, "\n")) != 10 {
			t.Fatalf("whole-card viewport lost selection %d:\n%s", selected, view)
		}
		if strings.Contains(view, "Activity idle · Activity") {
			t.Fatalf("duplicated activity: %s", view)
		}
	}
	jobs[0].Name = strings.Repeat("界", 100)
	if view := renderSessionList(jobs[:1], 0, 40, 4); !strings.Contains(view, "Process Live") {
		t.Fatalf("long name hid process state: %s", view)
	}
}

func TestDashboardResponsiveBoundsAndCompleteWideBorders(t *testing.T) {
	m := newModel(t.Context(), nil)
	m.jobs = []coordinator.JobFacts{{ID: "job", Name: "worker", Terminal: "live"}}
	for _, size := range []struct{ width, height int }{{120, 30}, {100, 30}, {96, 24}, {95, 24}, {80, 24}, {72, 12}, {71, 12}, {30, 12}, {20, 8}, {1, 1}, {160, 48}} {
		m.width, m.height = size.width, size.height
		view := m.View()
		if lipgloss.Width(view) > size.width || lipgloss.Height(view) > size.height {
			t.Errorf("%dx%d overflow:\n%s", size.width, size.height, view)
		}
		if size.width >= 96 && size.height >= 24 && strings.Count(view, "╰") != 2 {
			t.Errorf("%dx%d clipped pane borders:\n%s", size.width, size.height, view)
		}
	}
}

func TestSessionCardsColorProfilesKeepSemanticMarkers(t *testing.T) {
	previous := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(previous)
	attention := "review"
	jobs := []coordinator.JobFacts{{ID: "a", Name: "planner", Terminal: "live", Session: &coordinator.SessionStatus{Activity: "busy", Prompt: &coordinator.ObservedPrompt{Kind: "confirm"}, Report: &coordinator.AgentReport{Attention: &attention}}}}
	for _, profile := range []termenv.Profile{termenv.Ascii, termenv.ANSI} {
		lipgloss.SetColorProfile(profile)
		view := renderSessionList(jobs, 0, 44, 4)
		for _, required := range []string{"> planner", "[INPUT] [AGENT]", "Process Live"} {
			if !strings.Contains(ansi.Strip(view), required) {
				t.Errorf("profile %v lost %q: %s", profile, required, view)
			}
		}
		if profile == termenv.Ascii && strings.Contains(view, "\x1b") {
			t.Fatalf("monochrome emits styling: %q", view)
		}
		if profile == termenv.ANSI && (!strings.Contains(view, "\x1b[1;36m") || !strings.Contains(view, "\x1b[33m")) {
			t.Fatalf("limited color lost selection/attention hierarchy: %q", view)
		}
	}
}

func TestObservePrioritizesReadOnlyIdentityAndAttention(t *testing.T) {
	attention := "Please inspect layout"
	job := coordinator.JobFacts{ID: "123e4567-e89b-42d3-a456-426614174000", Name: "planner", Terminal: "live", Session: &coordinator.SessionStatus{Activity: "busy", Prompt: &coordinator.ObservedPrompt{Kind: "confirm", Title: "Review proposal?"}, Report: &coordinator.AgentReport{Summary: "Proposal ready", Attention: &attention}}}
	for _, visible := range []bool{true, false} {
		view := ansi.Strip(renderObserve(job, 80, 12, visible, "output-token", ""))
		for _, required := range []string{"Observe · READ ONLY", "planner", "123e4567", "Process Live", "Activity busy", "[INPUT]", "Review proposal?", "[AGENT]", "Please inspect layout", "Agent report: Proposal ready", "coverage unknown"} {
			if !strings.Contains(view, required) {
				t.Errorf("Observe visible=%v lost %q:\n%s", visible, required, view)
			}
		}
		if visible && (!strings.Contains(view, "Live output · current screen") || !strings.Contains(view, "output-token")) {
			t.Errorf("capture not labeled/shown:\n%s", view)
		}
		if !visible && (strings.Contains(view, "output-token") || !strings.Contains(view, "Output hidden · o show")) {
			t.Errorf("output toggle hid facts or exposed capture:\n%s", view)
		}
	}
	short := renderObserve(job, 72, 4, true, "must yield to warnings", "")
	for _, required := range []string{"[INPUT]", "[AGENT]", "coverage unknown"} {
		if !strings.Contains(short, required) {
			t.Errorf("short Observe lost priority %q:\n%s", required, short)
		}
	}
}

func TestDegradedDashboardKeepsBothAttentionSourcesWithActionStatus(t *testing.T) {
	attention := "review"
	m := newModel(t.Context(), nil)
	m.width, m.height = 30, 12
	m.jobs = []coordinator.JobFacts{{ID: "id", Name: "worker", Session: &coordinator.SessionStatus{Activity: "idle", Prompt: &coordinator.ObservedPrompt{Kind: "confirm"}, Report: &coordinator.AgentReport{Attention: &attention}}}}
	m.message = "Attach ended with error; inspect native session before retry."
	m.previewText = "optional capture"
	view := ansi.Strip(m.View())
	for _, required := range []string{"> worker", "[INPUT]", "[AGENT]", "Attach ended with error", "q quit"} {
		if !strings.Contains(view, required) {
			t.Errorf("degraded viewport lost priority %q:\n%s", required, view)
		}
	}
}

func TestCaptureTextCannotAcquireAttentionPresentation(t *testing.T) {
	previous := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(previous)
	lipgloss.SetColorProfile(termenv.ANSI)
	job := coordinator.JobFacts{ID: "id", Name: "worker", Terminal: "live", Session: &coordinator.SessionStatus{Activity: "idle"}}
	for _, marker := range []string{"[INPUT]", "[AGENT]"} {
		view := renderObserve(job, 48, 12, true, marker+" captured words "+strings.Repeat("output ", 20), "")
		if strings.Contains(view, "\x1b[33m") || strings.Contains(view, "Enter inspect") {
			t.Errorf("capture was presented as an observed request:\n%q", view)
		}
	}
	attention := "review"
	job.Session.Prompt = &coordinator.ObservedPrompt{Kind: "confirm"}
	job.Session.Report = &coordinator.AgentReport{Attention: &attention}
	view := ansi.Strip(renderObserve(job, 72, 5, true, "[INPUT] captured text, not a native request", ""))
	lines := strings.Split(view, "\n")
	if !strings.HasPrefix(lines[len(lines)-1], "Output · ") {
		t.Errorf("one-row capture has no source label:\n%s", view)
	}
	short := renderObserve(job, 72, 2, true, "optional output", "")
	if strings.Count(short, "\x1b[33m") != 2 || strings.Contains(short, "\x1b[1;36m") {
		t.Errorf("title-free warnings lost attention style: %q", short)
	}
}

func TestSingleLineCardsKeepUnselectedAttentionDiscoverable(t *testing.T) {
	attention := "review"
	jobs := []coordinator.JobFacts{
		{ID: "a", Name: "worker", Terminal: "live", Session: &coordinator.SessionStatus{Activity: "idle"}},
		{ID: "b", Name: "reviewer", Terminal: "live", Session: &coordinator.SessionStatus{Activity: "idle", Prompt: &coordinator.ObservedPrompt{Kind: "confirm"}, Report: &coordinator.AgentReport{Attention: &attention}}},
	}
	view := ansi.Strip(renderSessionCards(jobs, 0, 30, 3, 1))
	if !strings.Contains(view, "reviewer") || !strings.Contains(view, "[INPUT] [AGENT]") {
		t.Fatalf("unselected request disappeared from degraded list:\n%s", view)
	}
}

func TestNarrowActionStatusKeepsNoRetryAdvice(t *testing.T) {
	for _, message := range []string{"job publication uncertain; inspect list/doctor, do not retry", "launch uncertain; inspect list/doctor before any new attempt"} {
		m := newModel(t.Context(), nil)
		m.width, m.height, m.message = 30, 12, message
		text := strings.Join(strings.Fields(ansi.Strip(m.View())), " ")
		if !strings.Contains(text, message) || !strings.Contains(text, "q quit") {
			t.Errorf("recovery advice was lost:\n%s", m.View())
		}
	}
}

func TestObserveCompleteCompactRender(t *testing.T) {
	job := coordinator.JobFacts{ID: "id", Name: "worker", Terminal: "live", Session: &coordinator.SessionStatus{Activity: "idle"}}
	const expected = "Observe · READ ONLY · worker · id\nProcess Live · Activity idle\nNo observed input request · other coverage unknown\nLive output · current screen\nhello"
	if got := ansi.Strip(renderObserve(job, 80, 8, true, "hello", "")); got != expected {
		t.Fatalf("compact Observe:\n%s\nwant:\n%s", got, expected)
	}
}

func TestObserveSanitizesAllTextBeforeStyling(t *testing.T) {
	previous := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(previous)
	lipgloss.SetColorProfile(termenv.ANSI)
	malicious := "界e\u0301👩‍💻\x1b]52;c;payload\a\x1b[2J\r\x00\u0085\nSessions"
	job := coordinator.JobFacts{ID: malicious, Name: malicious, Terminal: "live", Session: &coordinator.SessionStatus{Activity: malicious, Prompt: &coordinator.ObservedPrompt{Kind: malicious, Title: malicious}, Report: &coordinator.AgentReport{Summary: malicious, Attention: &malicious}}}
	for _, width := range []int{30, 72, 120} {
		view := renderObserve(job, width, 16, true, "safe output\\x1b", "")
		plain := ansi.Strip(view)
		if strings.ContainsAny(plain, "\x1b\a\r\x00\u0085") || strings.Contains(view, "\x1b]52") || strings.Contains(view, "\x1b[2J") {
			t.Fatalf("executable payload in Observe: %q", view)
		}
		if strings.Contains(plain, "\nSessions") || lipgloss.Width(view) > width || lipgloss.Height(view) > 16 {
			t.Fatalf("text escaped its cell/row boundary:\n%s", view)
		}
	}
}

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
	if !strings.Contains(both, "Needs your input") || !strings.Contains(both, "[AGENT] Agent requests:") {
		t.Fatalf("native prompt and agent attention were not both shown:\n%s", both)
	}
	narrowBoth := renderObserve(job, 80, 4, true, "output", "")
	if !strings.Contains(narrowBoth, "[INPUT] Needs your input") || !strings.Contains(narrowBoth, "[AGENT] Agent requests:") {
		t.Fatalf("narrow layout hid simultaneous attention:\n%s", narrowBoth)
	}
	job.Session.Prompt = nil
	report := renderObserve(job, 80, 12, false, "", "")
	if plain := terminalStyles.ReplaceAllString(report, ""); !strings.Contains(plain, "[AGENT] Agent requests:") || !strings.Contains(plain, "needs review\\x1b") || strings.ContainsAny(plain, "\x1b\a\x00") {
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
	if view := m.View(); !strings.Contains(view, "Live output · current screen") || !strings.Contains(view, "\nok\n") {
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
