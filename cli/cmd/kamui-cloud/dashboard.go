package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

var dashboardTabs = [...]string{"Device", "Jobs", "Services", "Workspaces"}

type dashboardModel struct {
	app       *app
	ctx       context.Context
	tab       int
	selected  int
	requestID int
	items     []json.RawMessage
	detail    []string
	scroll    int
	width     int
	height    int
	loading   bool
	err       error
}

type dashboardReply struct {
	requestID int
	items     []json.RawMessage
	detail    []string
	err       error
}

func (a *app) dashboard(cmd *cobra.Command) error {
	model := dashboardModel{app: a, ctx: cmd.Context(), loading: true, height: 24, width: 80}
	program := tea.NewProgram(model, tea.WithContext(cmd.Context()), tea.WithInput(cmd.InOrStdin()),
		tea.WithOutput(cmd.OutOrStdout()), tea.WithAltScreen())
	_, err := program.Run()
	if errors.Is(err, tea.ErrProgramKilled) && cmd.Context().Err() != nil {
		return cmd.Context().Err()
	}
	return err
}

func (m dashboardModel) Init() tea.Cmd { return m.load() }

func (m dashboardModel) load() tea.Cmd {
	return func() tea.Msg {
		reply := dashboardReply{requestID: m.requestID}
		client, err := m.app.client()
		if err != nil {
			reply.err = err
			return reply
		}
		path := "/v1/workloads"
		if m.tab == 0 {
			path = "/v1/workers/local"
		} else if m.tab == 3 {
			path = workspacesPath
		}
		data, err := client.request(m.ctx, "GET", path, nil, "")
		switch {
		case err != nil:
			reply.err = err
		case m.tab == 0:
			reply.detail = jsonLines(data)
		case m.tab == 3:
			reply.err = json.Unmarshal(data, &reply.items)
		default:
			kind := "job"
			if m.tab == 2 {
				kind = "service"
			}
			reply.items, reply.err = filterWorkloads(data, kind)
		}
		return reply
	}
}

func (m dashboardModel) showLogs(id string) tea.Cmd {
	return func() tea.Msg {
		reply := dashboardReply{requestID: m.requestID, items: m.items}
		client, err := m.app.client()
		if err == nil {
			var raw json.RawMessage
			raw, err = client.request(m.ctx, "GET", "/v1/workloads/"+id+"/logs", nil, "")
			if err == nil {
				var logs struct{ Tail string }
				err = json.Unmarshal(raw, &logs)
				if err == nil {
					if logs.Tail == "" {
						logs.Tail = "No logs are available for this attempt."
					}
					reply.detail = strings.Split(terminalText(logs.Tail), "\n")
				}
			}
		}
		reply.err = err
		return reply
	}
}

func (m dashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = value.Width, value.Height
	case dashboardReply:
		if value.requestID == m.requestID {
			m.items, m.detail, m.err, m.loading = value.items, value.detail, value.err, false
			m.selected = min(m.selected, max(0, len(m.items)-1))
			m.scroll = 0
		}
	case tea.KeyMsg:
		switch key := value.String(); key {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "1", "2", "3", "4", "tab", "shift+tab", "r":
			switch key {
			case "tab":
				m.tab = (m.tab + 1) % len(dashboardTabs)
			case "shift+tab":
				m.tab = (m.tab + len(dashboardTabs) - 1) % len(dashboardTabs)
			case "r":
			default:
				m.tab = int(key[0] - '1')
			}
			m.selected, m.scroll = 0, 0
			m.items, m.detail, m.err, m.loading = nil, nil, nil, true
			m.requestID++
			return m, m.load()
		case "up", "k":
			if m.detail != nil {
				m.scroll = max(0, m.scroll-1)
			} else {
				m.selected = max(0, m.selected-1)
			}
		case "down", "j":
			if m.detail != nil {
				m.scroll = min(m.scroll+1, max(0, len(m.detail)-m.rows()))
			} else {
				m.selected = min(m.selected+1, max(0, len(m.items)-1))
			}
		case "esc":
			if m.tab != 0 {
				m.detail, m.scroll = nil, 0
				if m.loading && m.items != nil {
					m.requestID++
					m.loading = false
				}
			}
		case "enter":
			if !m.loading && len(m.items) > 0 && m.detail == nil {
				m.detail, m.scroll = jsonLines(m.items[m.selected]), 0
			}
		case "l":
			if !m.loading && (m.tab == 1 || m.tab == 2) && len(m.items) > 0 {
				var job workloadSummary
				if json.Unmarshal(m.items[m.selected], &job) == nil && workloadID.MatchString(job.ID) {
					m.requestID++
					m.loading = true
					return m, m.showLogs(job.ID)
				}
			}
		}
	}
	return m, nil
}

func (m dashboardModel) rows() int { return max(1, m.height-6) }

func (m dashboardModel) View() string {
	var view strings.Builder
	view.WriteString("Kamui Cloud\n\n")
	for i, tab := range dashboardTabs {
		if i == m.tab {
			fmt.Fprintf(&view, "[%d %s]  ", i+1, tab)
		} else {
			fmt.Fprintf(&view, "%d %s  ", i+1, tab)
		}
	}
	view.WriteString("\n\n")
	switch {
	case m.loading:
		view.WriteString("Loading…\n")
	case m.err != nil:
		view.WriteString(terminalText(m.err.Error()) + "\n")
	case m.detail != nil:
		start := min(m.scroll, max(0, len(m.detail)-m.rows()))
		for _, line := range m.detail[start:min(len(m.detail), start+m.rows())] {
			view.WriteString(truncate(line, m.width) + "\n")
		}
	case len(m.items) == 0:
		view.WriteString("No " + strings.ToLower(dashboardTabs[m.tab]) + " found.\n")
	default:
		start := max(0, m.selected-m.rows()+1)
		for i := start; i < min(len(m.items), start+m.rows()); i++ {
			cursor := "  "
			if m.selected == i {
				cursor = "> "
			}
			view.WriteString(truncate(cursor+dashboardRow(m.items[i]), m.width) + "\n")
		}
	}
	view.WriteString("\n1–4 views · ↑/↓ navigate · Enter details · l logs · Esc back · r refresh · q quit")
	return view.String()
}

func jsonLines(raw json.RawMessage) []string {
	var out strings.Builder
	if err := printJSON(&out, raw, false); err != nil {
		return []string{"Invalid coordinator response"}
	}
	return strings.Split(strings.TrimSuffix(terminalText(out.String()), "\n"), "\n")
}

func dashboardRow(raw json.RawMessage) string {
	var item struct {
		workloadSummary
		Name, Status string
	}
	if json.Unmarshal(raw, &item) != nil {
		return "Invalid coordinator response"
	}
	name, state := item.Name, item.Status
	if item.Submission.Image != "" {
		name, state = item.Submission.Image, item.State
	}
	return terminalText(item.ID + "  " + state + "  " + name)
}

func truncate(line string, width int) string {
	runes := []rune(line)
	if width > 3 && len(runes) > width {
		return string(runes[:width-1]) + "…"
	}
	return line
}
