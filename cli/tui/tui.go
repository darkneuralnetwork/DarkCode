package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	colorBg       = lipgloss.Color("#0f1115")
	colorSurface  = lipgloss.Color("#1a1d24")
	colorPrimary  = lipgloss.Color("#0ea5e9") // Electric Blue
	colorSuccess  = lipgloss.Color("#10b981") // Green
	colorWarning  = lipgloss.Color("#f59e0b") // Amber
	colorText     = lipgloss.Color("#e2e8f0")
	colorMuted    = lipgloss.Color("#64748b")
	colorBorder   = lipgloss.Color("#272c36")

	styleBase = lipgloss.NewStyle().
			Foreground(colorText).
			Background(colorBg)

	styleHeader = lipgloss.NewStyle().
			Foreground(colorPrimary).
			Bold(true).
			Border(lipgloss.NormalBorder(), false, false, true, false).
			BorderForeground(colorBorder).
			Padding(0, 1)

	stylePanel = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorBorder).
			Padding(0, 1)
)

type Model struct {
	width  int
	height int
	logs   []string
}

func InitialModel() Model {
	return Model{
		logs: []string{"System initialized...", "Loading Kernel...", "Connected to DarkCode Core"},
	}
}

func (m Model) Init() tea.Cmd {
	return nil
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m Model) View() string {
	if m.width == 0 {
		return "Initializing..."
	}

	header := styleHeader.Width(m.width).Render("🚀 DARKCODE ENTERPRISE PLATFORM (CLI)")

	sidebarWidth := m.width / 4
	mainWidth := m.width - sidebarWidth - 4 // accounting for borders

	sidebar := stylePanel.Width(sidebarWidth).Height(m.height - 4).Render(
		"📂 WORKSPACE\n\n" +
			"▸ src/\n" +
			"▸ pkg/\n" +
			"▸ cmd/\n\n" +
			"🧠 MEMORY\n\n" +
			"▸ Context: 12%\n" +
			"▸ Cache: Active",
	)

	mainContent := "💬 CHAT CONSOLE\n\n"
	for _, l := range m.logs {
		mainContent += fmt.Sprintf(" %s %s\n", lipgloss.NewStyle().Foreground(colorSuccess).Render("✓"), l)
	}

	mainPanel := stylePanel.Width(mainWidth).Height(m.height - 4).Render(mainContent)

	layout := lipgloss.JoinHorizontal(lipgloss.Top, sidebar, mainPanel)

	return styleBase.Render(lipgloss.JoinVertical(lipgloss.Left, header, layout))
}

func Run() error {
	p := tea.NewProgram(InitialModel(), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
