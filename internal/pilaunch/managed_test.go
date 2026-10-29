package pilaunch

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func managedFixture(t *testing.T) Managed {
	t.Helper()
	root := t.TempDir()
	agent := filepath.Join(root, "agent")
	launcher := filepath.Join(agent, "bin", "pi")
	if err := os.MkdirAll(filepath.Dir(launcher), 0700); err != nil {
		t.Fatal(err)
	}
	launcherBytes, err := os.ReadFile("testdata/pi")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(launcherBytes)
	if hex.EncodeToString(sum[:]) != launcherSHA256 {
		t.Fatal("fixture launcher bytes differ from the approved pinned launcher")
	}
	if err := os.WriteFile(launcher, launcherBytes, 0700); err != nil {
		t.Fatal(err)
	}
	install := filepath.Join(agent, "install")
	release := filepath.Join(install, "releases", "1.4.0")
	packageDir := filepath.Join(release, "node_modules", "@earendil-works", "pi-coding-agent")
	entry := filepath.Join(packageDir, "dist", "bundle", "cli.js")
	binDir := filepath.Join(release, "node_modules", ".bin")
	for _, dir := range []string{packageDir, filepath.Dir(entry), binDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(install, "current-version"), []byte("1.4.0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "package.json"), []byte(`{"name":"@earendil-works/pi-coding-agent","version":"1.4.0","bin":{"pi":"dist/bundle/cli.js"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte("#!/usr/bin/env node\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../@earendil-works/pi-coding-agent/dist/bundle/cli.js", filepath.Join(binDir, "pi")); err != nil {
		t.Fatal(err)
	}
	managed, err := ObserveManaged(launcher)
	if err != nil {
		t.Fatalf("observe exact frozen managed layout: %v", err)
	}
	return managed
}

func TestObserveManagedRecognizedFrozenLayout(t *testing.T) {
	managed := managedFixture(t)
	if managed.Observation.Version != "1.4.0" {
		t.Fatalf("observed version %q", managed.Observation.Version)
	}
	if err := Recheck(managed); err != nil {
		t.Fatalf("stable observation failed recheck: %v", err)
	}
	launcher, err := filepath.EvalSymlinks(managed.Executable)
	if err != nil {
		t.Fatal(err)
	}
	if managed.Observation.BinLink.Path != filepath.Join(filepath.Dir(filepath.Dir(launcher)), "install", "releases", "1.4.0", "node_modules", ".bin", "pi") {
		t.Fatalf("wrong observed managed bin: %s", managed.Observation.BinLink.Path)
	}
}

func TestRecheckRejectsVersionAndBinLinkDrift(t *testing.T) {
	t.Run("launcher hash", func(t *testing.T) {
		managed := managedFixture(t)
		if err := os.WriteFile(managed.Executable, []byte("changed"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := Recheck(managed); err == nil {
			t.Fatal("launcher hash drift accepted")
		}
	})
	t.Run("version", func(t *testing.T) {
		managed := managedFixture(t)
		current := filepath.Join(filepath.Dir(filepath.Dir(managed.Executable)), "install", "current-version")
		if err := os.WriteFile(current, []byte("1.5.0\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := Recheck(managed); err == nil {
			t.Fatal("version drift accepted")
		}
	})
	t.Run("package metadata", func(t *testing.T) {
		managed := managedFixture(t)
		packagePath := managed.Observation.Package.Path
		if err := os.WriteFile(packagePath, []byte(`{"name":"@earendil-works/pi-coding-agent","version":"1.4.0","bin":{"pi":"dist/other.js"}}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := Recheck(managed); err == nil {
			t.Fatal("package metadata drift accepted")
		}
	})
	t.Run("bin link", func(t *testing.T) {
		managed := managedFixture(t)
		link := managed.Observation.BinLink.Path
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		wrong := filepath.Join(filepath.Dir(filepath.Dir(link)), "@earendil-works", "pi-coding-agent", "dist", "other.js")
		if err := os.MkdirAll(filepath.Dir(wrong), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(wrong, []byte("other"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../@earendil-works/pi-coding-agent/dist/other.js", link); err != nil {
			t.Fatal(err)
		}
		if err := Recheck(managed); err == nil {
			t.Fatal("bin-link drift accepted")
		}
	})
}

func TestObserveRejectsUnrecognizedLauncherWithoutExecution(t *testing.T) {
	launcher := filepath.Join(t.TempDir(), "pi")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Observe(launcher); err == nil {
		t.Fatal("unrecognized launcher was accepted")
	}
}

func TestResolveRejectsCoordinatorIdentity(t *testing.T) {
	coordinator := filepath.Join(t.TempDir(), "agent-coordinator")
	if err := os.WriteFile(coordinator, []byte("binary"), 0700); err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(coordinator, "", coordinator)
	if err == nil || got != "" {
		t.Fatalf("coordinator recursion accepted: %q, %v", got, err)
	}
}

func TestBinLinkMustResolveToDeclaredEntry(t *testing.T) {
	root := t.TempDir()
	declared := filepath.Join(root, "declared")
	other := filepath.Join(root, "other")
	link := filepath.Join(root, "bin")
	for _, path := range []string{declared, other} {
		if err := os.WriteFile(path, []byte(path), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if sameResolvedEntry(link, declared) {
		t.Fatal("mismatched managed bin link accepted")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(declared, link); err != nil {
		t.Fatal(err)
	}
	if !sameResolvedEntry(link, declared) {
		t.Fatal("valid declared bin link rejected")
	}
}

func TestMetadataReadRejectsSymlinkAndFIFO(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := observeFile(filepath.Join(root, "link"), 10); err == nil {
		t.Fatal("metadata symlink accepted")
	}
	fifo := filepath.Join(root, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := observeFile(fifo, 10); err == nil {
		t.Fatal("metadata FIFO accepted")
	}
}

func TestObservationIsImmutableAndBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata")
	body := []byte("safe")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := observeFile(path, 4)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if got.Hash != hex.EncodeToString(sum[:]) || got.Size != 4 {
		t.Fatalf("unexpected observation: %+v", got)
	}
	if _, err := observeFile(path, 3); err == nil {
		t.Fatal("oversized metadata accepted")
	}
	if strings.Contains(got.Hash, " ") {
		t.Fatal("invalid digest")
	}
}
