package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/cellbuf"
)

func overlayHelp(background string, width, height int) string {
	panel := renderHelp(width, height)
	panelWidth, panelHeight := lipgloss.Width(panel), lipgloss.Height(panel)
	left, top := max((width-panelWidth)/2, 0), max((height-panelHeight)/2, 0)
	buffer := cellbuf.NewBuffer(max(width, 1), max(height, 1))
	cellbuf.SetContent(buffer, background)
	cellbuf.SetContentRect(buffer, panel, cellbuf.Rect(left, top, panelWidth, panelHeight))
	view := strings.ReplaceAll(cellbuf.Render(buffer), "\r\n", "\n")
	return lipgloss.NewStyle().Width(max(width, 1)).Height(max(height, 1)).Render(view)
}

func renderHelp(width, height int) string {
	const content = "Keyboard help\n↑/↓ select session\nEnter preview, then open session\no toggle Observe output\nn create new task\nm steer · send direction literally\ns request graceful stop\nX force-stop (may interrupt writes)\nd details\nc process Continue\nr repair socket\nf Forget ended job\nl refresh session facts\nq / Esc / h close help\nCtrl+C quit\nF12 Return from attachment"
	width, height = max(width, 1), max(height, 1)
	style := lipgloss.NewStyle().Padding(1, 2).Border(lipgloss.RoundedBorder()).BorderForeground(inkBorder).Foreground(inkText).Background(inkPanel)
	frameWidth, frameHeight := style.GetFrameSize()
	panelWidth := min(width, 46)
	if panelWidth <= frameWidth || height < strings.Count(content, "\n")+1+frameHeight {
		return clipView("Help · resize to view bindings", width, height)
	}
	style = style.Width(panelWidth - frameWidth)
	return style.Render(content)
}
