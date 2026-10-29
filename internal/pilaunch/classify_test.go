package pilaunch

import "testing"

func TestClassifyConsumesPiValuesBeforeRecognizingPrint(t *testing.T) {
	got := Classify([]string{"--append-system-prompt", "--print", "hello"}, true)
	if got.Kind != Wrapped {
		t.Fatalf("value resembling a flag changed routing: got %s (%s)", got.Kind, got.Reason)
	}
}

func TestClassifyUsesFinalModeAndStickyHelpPrintPrecedence(t *testing.T) {
	for _, fixture := range []struct {
		name string
		argv []string
		want Route
	}{
		{"json then text", []string{"--mode", "json", "--mode", "text"}, Wrapped},
		{"text then rpc", []string{"--mode", "text", "--mode", "rpc"}, Passthrough},
		{"help stays sticky", []string{"--help", "--mode", "text"}, Passthrough},
		{"print stays sticky", []string{"--print", "--mode", "text"}, Passthrough},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			got := Classify(fixture.argv, true)
			if got.Kind != fixture.want {
				t.Fatalf("got %s (%s), want %s", got.Kind, got.Reason, fixture.want)
			}
		})
	}
}

func TestClassifySupportedAndRefusedGrammar(t *testing.T) {
	for _, fixture := range []struct {
		name string
		argv []string
		want Route
	}{
		{"literal after delimiter", []string{"--", "--resume"}, Wrapped},
		{"repeated values", []string{"--theme", "a", "--theme", "b"}, Wrapped},
		{"print bypass", []string{"-p", "hello"}, Passthrough},
		{"selector refuses", []string{"--session", "x"}, Refused},
		{"unknown refuses", []string{"--extensionx"}, Refused},
		{"missing operand refuses", []string{"--model"}, Refused},
		{"no session bypass", []string{"--no-session"}, Passthrough},
		{"no extensions bypass", []string{"-ne"}, Passthrough},
		{"empty value consumed", []string{"--model", ""}, Wrapped},
		{"flag-like provider consumed", []string{"--provider", "--print"}, Wrapped},
		{"tui mode rejects flag-like value", []string{"--tui-mode", "--print"}, Refused},
		{"noneligible passthrough", []string{"--resume"}, Passthrough},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			got := Classify(fixture.argv, fixture.name != "noneligible passthrough")
			if got.Kind != fixture.want {
				t.Fatalf("got %s (%s), want %s", got.Kind, got.Reason, fixture.want)
			}
		})
	}
}
