package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type SelectorItem struct {
	Title       string
	Description string
	Value       string
}

type selectorModel struct {
	items    []SelectorItem
	filtered []SelectorItem
	cursor   int
	filter   string
	selected string
	title    string
}

func (m selectorModel) Init() tea.Cmd {
	return nil
}

func (m *selectorModel) applyFilter() {
	if m.filter == "" {
		m.filtered = m.items
	} else {
		m.filtered = []SelectorItem{}
		lowerFilter := strings.ToLower(m.filter)
		for _, item := range m.items {
			if strings.Contains(strings.ToLower(item.Title), lowerFilter) || strings.Contains(strings.ToLower(item.Value), lowerFilter) || strings.Contains(strings.ToLower(item.Description), lowerFilter) {
				m.filtered = append(m.filtered, item)
			}
		}
	}
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	if m.cursor < 0 && len(m.filtered) > 0 {
		m.cursor = 0
	}
}

func (m selectorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			m.selected = ""
			return m, tea.Quit
		case tea.KeyUp:
			if m.cursor > 0 {
				m.cursor--
			} else {
				m.cursor = len(m.filtered) - 1
			}
		case tea.KeyDown:
			if m.cursor < len(m.filtered)-1 {
				m.cursor++
			} else {
				m.cursor = 0
			}
		case tea.KeyEnter:
			if len(m.filtered) > 0 {
				m.selected = m.filtered[m.cursor].Value
			} else {
				m.selected = ""
			}
			return m, tea.Quit
		case tea.KeyBackspace, tea.KeyDelete:
			if len(m.filter) > 0 {
				m.filter = m.filter[:len(m.filter)-1]
				m.applyFilter()
			}
		default:
			if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
				m.filter += msg.String()
				m.applyFilter()
			}
		}
	}
	return m, nil
}

func (m selectorModel) View() string {
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#0ea5e9")).MarginBottom(1)
	searchStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#10b981"))
	listStyle := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#272c36")).Padding(0, 1)
	
	s := titleStyle.Render("? " + m.title) + "\n"
	s += searchStyle.Render("🔍 Filter: ") + m.filter + "\n\n"

	var listContent string
	if len(m.filtered) == 0 {
		listContent = lipgloss.NewStyle().Foreground(lipgloss.Color("#64748b")).Render("No matches found.")
	} else {
		// Pagination (show up to 10 items)
		start := 0
		end := len(m.filtered)
		if len(m.filtered) > 10 {
			start = m.cursor - 4
			if start < 0 {
				start = 0
			}
			end = start + 10
			if end > len(m.filtered) {
				end = len(m.filtered)
				start = end - 10
			}
		}

		for i := start; i < end; i++ {
			item := m.filtered[i]
			cursor := "  "
			itemTitleStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#e2e8f0"))
			descStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#64748b"))
			
			if m.cursor == i {
				cursor = lipgloss.NewStyle().Foreground(lipgloss.Color("#0ea5e9")).Render("❯ ")
				itemTitleStyle = itemTitleStyle.Foreground(lipgloss.Color("#0ea5e9")).Bold(true)
			}
			
			listContent += fmt.Sprintf("%s%s %s\n", cursor, itemTitleStyle.Render(item.Title), descStyle.Render(item.Description))
		}
		if len(m.filtered) > 10 {
			listContent += lipgloss.NewStyle().Foreground(lipgloss.Color("#64748b")).Render(fmt.Sprintf("\n... %d more items", len(m.filtered)-10))
		}
	}

	s += listStyle.Render(listContent)
	s += lipgloss.NewStyle().Foreground(lipgloss.Color("#64748b")).Render("\n\n(Type to filter, ↑/↓ to move, Enter to select, Esc to cancel)")

	return s
}

// Select shows an interactive list and returns the selected item's Value, or empty string if cancelled.
func Select(title string, items []SelectorItem) string {
	if len(items) == 0 {
		return ""
	}
	initialModel := selectorModel{
		items:    items,
		filtered: items,
		title:    title,
		cursor:   0,
	}
	p := tea.NewProgram(initialModel)
	m, err := p.Run()
	if err != nil {
		return ""
	}
	if finalModel, ok := m.(selectorModel); ok {
		return finalModel.selected
	}
	return ""
}
