package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type inputModel struct {
	prompt   string
	value    string
	password bool
	canceled bool
}

func (m inputModel) Init() tea.Cmd {
	return nil
}

func (m inputModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			m.canceled = true
			return m, tea.Quit
		case tea.KeyEnter:
			return m, tea.Quit
		case tea.KeyBackspace, tea.KeyDelete:
			if len(m.value) > 0 {
				m.value = m.value[:len(m.value)-1]
			}
		default:
			if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
				m.value += msg.String()
			}
		}
	}
	return m, nil
}

func (m inputModel) View() string {
	promptStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#f97316")).Bold(true)
	textStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#e2e8f0"))

	displayValue := m.value
	if m.password && len(displayValue) > 0 {
		displayValue = strings.Repeat("*", len(displayValue))
	}
	
	// Blinking cursor simulation (static for simplicity unless we add a tick, but this is fine)
	displayValue += "█"

	return promptStyle.Render("? ") + lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#f8fafc")).Render(m.prompt) + " " + textStyle.Render(displayValue) + "\n"
}

// Input shows an interactive text input prompt and returns the string, and a boolean indicating if it was cancelled.
func Input(prompt string, password bool) (string, bool) {
	initialModel := inputModel{
		prompt:   prompt,
		value:    "",
		password: password,
	}
	p := tea.NewProgram(initialModel)
	m, err := p.Run()
	if err != nil {
		return "", true
	}
	if finalModel, ok := m.(inputModel); ok {
		if finalModel.canceled {
			return "", true
		}
		return finalModel.value, false
	}
	return "", true
}
