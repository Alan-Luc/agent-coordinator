package tui

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

func TestRenderPreviewKeepsOnlyBoundedSGRAndResetsLines(t *testing.T) {
	input := "\x1b[38;2;220;215;186;48;5;240;1mwide 世界\ncontinued\x1b[0m\x1b[2Jdone"
	got := renderPreview(input, 40)
	if !strings.Contains(got, "\x1b[38;2;220;215;186;48;5;240;1mwide 世界\x1b[0m\n") {
		t.Fatalf("lost validated color/style or line reset: %q", got)
	}
	if !strings.Contains(got, "\x1b[1;38;2;220;215;186;48;5;240mcontinued\x1b[0m") || strings.Contains(got, "\x1b[2J") {
		t.Fatalf("multiline style inheritance or unsafe control handling wrong: %q", got)
	}
}

func TestRenderPreviewRejectsUnsafeAndOutOfBoundsSGR(t *testing.T) {
	got := renderPreview("a\x1b[8mconcealed\x1b[38;5;256m bad\x1b[38;5;-1m negative\x1b[38;2;256;0;0m rgb\x1b]52;c;x\aOSC\x1bPdata\x1b\\DCS\x1b[2Jz\u0085end", 80)
	if strings.ContainsAny(got, "\x1b\a") {
		t.Fatalf("unsafe styling/control survived: %q", got)
	}
	if !strings.Contains(ansi.Strip(got), "aconcealed bad1m negative rgbOSCDCSzend") {
		t.Fatalf("safe text was not retained: %q", got)
	}
}

func TestRenderPreviewZeroPaddedRGBModeCarriesForegroundAndBackground(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{"foreground", "\x1b[38;02;12;34;56mone\ncontinued", "\x1b[38;02;12;34;56mcontinued\x1b[0m"},
		{"background", "\x1b[48;02;12;34;56mone\ncontinued", "\x1b[48;02;12;34;56mcontinued\x1b[0m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderPreview(tc.input, 40); !strings.Contains(got, tc.want) {
				t.Fatalf("zero-padded RGB mode was not carried: %q", got)
			}
		})
	}
}

func TestRenderPreviewCombinedResetCarriesFollowingColor(t *testing.T) {
	got := renderPreview("\x1b[0;31mred\ncontinued", 40)
	if !strings.Contains(got, "\x1b[31mcontinued\x1b[0m") {
		t.Fatalf("color after combined reset was not carried: %q", got)
	}
}

func TestRenderPreviewRejectsRawC1AndInvalidUTF8(t *testing.T) {
	input := string([]byte{'a', 0x9b, '3', '1', 'm', 'b', 0x9d, '5', '2', ';', 'c', ';', 'x', 0x07, 'c', 0xff, 'd'}) + "e\u0301\u202e"
	got := renderPreview(input, 80)
	if strings.ContainsAny(got, "\x9b\x9d\x07\xff") || !utf8.ValidString(got) {
		t.Fatalf("raw controls or invalid UTF-8 survived: %q", got)
	}
	if !strings.Contains(ansi.Strip(got), "abcd") || !strings.Contains(got, "e\u0301") || strings.Contains(got, "\u202e") {
		t.Fatalf("printable text was lost: %q", got)
	}
}

func TestRenderPreviewCarriesOnlyActiveStyles(t *testing.T) {
	input := strings.Repeat("\x1b[31m\x1b[32m", 1000) + "text\ncontinued"
	got := renderPreview(input, 80)
	lines := strings.Split(got, "\n")
	if len(lines) != 2 || strings.Count(lines[1], "\x1b[") != 2 || !strings.HasPrefix(lines[1], "\x1b[32mcontinued") {
		t.Fatalf("carried style history was not bounded to active color: %q", lines)
	}
}

func TestRenderPreviewClippingResetsStyleAndKeepsCellWidth(t *testing.T) {
	got := renderPreview("\x1b[31;1m界界界", 3)
	if strings.Contains(got, "\x1b[31;1m") && !strings.HasSuffix(got, "\x1b[0m") {
		t.Fatalf("clipped style leaked beyond preview line: %q", got)
	}
	if ansi.StringWidth(got) > 3 {
		t.Fatalf("preview exceeds cell width: %q", got)
	}
}
