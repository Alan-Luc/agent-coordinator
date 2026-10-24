package tui

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

func renderPreview(output string, width int) string {
	if width < 1 {
		return ""
	}
	output = strings.ReplaceAll(output, "\r\n", "\n")
	var lines []string
	var line strings.Builder
	var style previewStyle
	carry := ""
	state := ansi.NormalState
	parser := ansi.NewParser()
	for len(output) > 0 {
		seq, _, n, next := ansi.DecodeSequenceWc(output, state, parser)
		if n == 0 {
			break
		}
		output, state = output[n:], next
		if state != ansi.NormalState {
			continue
		}
		if seq == "\n" {
			lines = append(lines, truncatePreviewLine(carry+line.String(), width))
			line.Reset()
			carry = style.sequence()
			continue
		}
		if len(seq) > 0 && seq[0] == '\x1b' {
			if validPreviewSGR(seq) {
				line.WriteString(seq)
				style.apply(seq)
			}
			continue
		}
		if !utf8.ValidString(seq) {
			continue
		}
		graphic := true
		for _, r := range seq {
			if !unicode.IsGraphic(r) {
				graphic = false
				break
			}
		}
		if graphic {
			line.WriteString(seq)
		}
	}
	lines = append(lines, truncatePreviewLine(carry+line.String(), width))
	return strings.Join(lines, "\n")
}

func truncatePreviewLine(line string, width int) string {
	truncated := ansi.Truncate(line, width, "…")
	if strings.Contains(truncated, "\x1b[") {
		return truncated + "\x1b[0m"
	}
	return truncated
}

type previewStyle struct {
	bold, faint, italic, underline bool
	foreground, background         string
}

func (s *previewStyle) apply(seq string) {
	params := strings.Split(seq[2:len(seq)-1], ";")
	if len(params) == 1 && params[0] == "" {
		params[0] = "0"
	}
	for i := 0; i < len(params); i++ {
		value, _ := strconv.Atoi(params[i])
		switch value {
		case 0:
			*s = previewStyle{}
		case 1:
			s.bold = true
		case 2:
			s.faint = true
		case 3:
			s.italic = true
		case 4:
			s.underline = true
		case 22:
			s.bold, s.faint = false, false
		case 23:
			s.italic = false
		case 24:
			s.underline = false
		case 39:
			s.foreground = ""
		case 49:
			s.background = ""
		case 38, 48:
			end := i + 2
			mode, _ := strconv.Atoi(params[i+1])
			if mode == 2 {
				end = i + 4
			}
			color := strings.Join(params[i:end+1], ";")
			if value == 38 {
				s.foreground = color
			} else {
				s.background = color
			}
			i = end
		default:
			if value >= 30 && value <= 37 || value >= 90 && value <= 97 {
				s.foreground = params[i]
			} else if value >= 40 && value <= 47 || value >= 100 && value <= 107 {
				s.background = params[i]
			}
		}
	}
}

func (s previewStyle) sequence() string {
	var params []string
	if s.bold {
		params = append(params, "1")
	}
	if s.faint {
		params = append(params, "2")
	}
	if s.italic {
		params = append(params, "3")
	}
	if s.underline {
		params = append(params, "4")
	}
	if s.foreground != "" {
		params = append(params, s.foreground)
	}
	if s.background != "" {
		params = append(params, s.background)
	}
	if len(params) == 0 {
		return ""
	}
	return "\x1b[" + strings.Join(params, ";") + "m"
}

func validPreviewSGR(seq string) bool {
	if !strings.HasPrefix(seq, "\x1b[") || !strings.HasSuffix(seq, "m") {
		return false
	}
	params := strings.Split(seq[2:len(seq)-1], ";")
	if len(params) == 1 && params[0] == "" {
		return true
	}
	for i := 0; i < len(params); i++ {
		value, err := strconv.Atoi(params[i])
		if err != nil || value < 0 || value > 255 {
			return false
		}
		switch value {
		case 0, 1, 2, 3, 4, 22, 23, 24, 39, 49:
		case 30, 31, 32, 33, 34, 35, 36, 37, 40, 41, 42, 43, 44, 45, 46, 47, 90, 91, 92, 93, 94, 95, 96, 97, 100, 101, 102, 103, 104, 105, 106, 107:
		case 38, 48:
			if i+2 >= len(params) {
				return false
			}
			mode, modeErr := strconv.Atoi(params[i+1])
			if modeErr != nil {
				return false
			}
			if mode == 5 {
				index, indexErr := strconv.Atoi(params[i+2])
				if indexErr != nil || index < 0 || index > 255 {
					return false
				}
				i += 2
			} else if mode == 2 && i+4 < len(params) {
				for _, component := range params[i+2 : i+5] {
					color, colorErr := strconv.Atoi(component)
					if colorErr != nil || color < 0 || color > 255 {
						return false
					}
				}
				i += 4
			} else {
				return false
			}
		default:
			return false
		}
	}
	return true
}
