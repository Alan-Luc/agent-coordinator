//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

func TestRealCLIListsTwentyConcurrentJobsFromOneInstallation(t *testing.T) {
	requireTmux(t)
	sandbox := buildBinaries(t)
	const count = 20
	jobs := make([]*fakeJob, count)
	for i := range jobs {
		jobs[i] = newSharedFakeJob(t, sandbox, fmt.Sprintf("scale-job-%02d", i))
		registerSharedCleanup(t, jobs[i])
	}

	type result struct {
		index int
		facts launchFacts
		err   error
		out   []byte
	}
	results := make(chan result, count)
	var group sync.WaitGroup
	for i, job := range jobs {
		group.Add(1)
		go func(index int, job *fakeJob) {
			defer group.Done()
			cmd := exec.Command(job.cli, "start", job.facts.Name, "--cwd", job.cwd, "--task-file", "-", "--json")
			cmd.Env = job.env
			cmd.Stdin = strings.NewReader("scale fixture")
			output, err := cmd.CombinedOutput()
			var facts launchFacts
			if err == nil {
				err = json.Unmarshal(output, &facts)
			}
			results <- result{index: index, facts: facts, err: err, out: output}
		}(i, job)
	}
	group.Wait()
	close(results)
	seen := make(map[string]bool, count)
	for result := range results {
		if result.err != nil {
			t.Errorf("concurrent start %s: %v: %s", jobs[result.index].facts.Name, result.err, result.out)
			continue
		}
		if result.facts.ID == "" || result.facts.Name != jobs[result.index].facts.Name || result.facts.Launch != "submitted" || result.facts.Terminal != "live" {
			t.Errorf("unexpected launch facts for %s: %+v", jobs[result.index].facts.Name, result.facts)
			continue
		}
		if seen[result.facts.ID] {
			t.Errorf("concurrent jobs reused UUID %s", result.facts.ID)
		}
		seen[result.facts.ID] = true
		jobs[result.index].facts = result.facts
		jobs[result.index].socket = socketFromManifest(t, jobs[result.index])
	}
	if t.Failed() {
		return
	}
	if len(seen) != count {
		t.Fatalf("accepted %d unique jobs, want %d", len(seen), count)
	}

	var listed struct {
		Jobs []launchFacts `json:"jobs"`
	}
	output := runCLIJSON(t, sandbox.cli, jobs[0].env, "list", "--json")
	if err := json.Unmarshal(output, &listed); err != nil {
		t.Fatalf("decode real CLI list: %v: %s", err, output)
	}
	found := make(map[string]launchFacts, len(listed.Jobs))
	for _, job := range listed.Jobs {
		found[job.ID] = job
	}
	for _, job := range jobs {
		fact, ok := found[job.facts.ID]
		if !ok || fact.Name != job.facts.Name || fact.Launch != "submitted" || fact.Terminal != "live" {
			t.Errorf("shared installation list omitted/misreported %s: found=%t facts=%+v", job.facts.Name, ok, fact)
		}
		if capture := waitForCapture(t, job.capturePath); capture.Cwd != job.cwd || capture.Body != taskHeader+"scale fixture" {
			t.Errorf("fake Pi observed incorrect job identity/body for %s: cwd=%q body=%q", job.facts.Name, capture.Cwd, capture.Body)
		}
	}
	for _, job := range jobs {
		if err := os.WriteFile(job.release, nil, 0600); err != nil {
			t.Errorf("release owned fake Pi %s: %v", job.facts.Name, err)
		}
	}
}
