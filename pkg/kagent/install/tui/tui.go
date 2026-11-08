// Copyright 2026 The Agent Substrate Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package tui renders installer progress as a live step list.
package tui

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"go.ate.dev/substrate/pkg/kagent/install"
)

// progressLines is how many of a running step's latest progress lines show.
const progressLines = 3

var (
	accent  = lipgloss.AdaptiveColor{Light: "#5A3FC0", Dark: "#A78BFA"}
	green   = lipgloss.AdaptiveColor{Light: "#1A7F37", Dark: "#3FB950"}
	red     = lipgloss.AdaptiveColor{Light: "#CF222E", Dark: "#F85149"}
	yellow  = lipgloss.AdaptiveColor{Light: "#9A6700", Dark: "#D29922"}
	subtle  = lipgloss.AdaptiveColor{Light: "#6E7781", Dark: "#8B949E"}
	titleSt = lipgloss.NewStyle().Bold(true).Foreground(accent)
	dimSt   = lipgloss.NewStyle().Foreground(subtle)
	okSt    = lipgloss.NewStyle().Foreground(green)
	errSt   = lipgloss.NewStyle().Foreground(red)
	warnSt  = lipgloss.NewStyle().Foreground(yellow)
	boxSt   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1).MarginTop(1)
)

type state int

const (
	pending state = iota
	running
	done
	failed
)

type stepView struct {
	name     string
	state    state
	msg      string
	progress []string
	started  time.Time
	elapsed  time.Duration
}

type eventMsg install.Event

type finishMsg struct {
	err     error
	summary string
}

type model struct {
	title     string
	notes     []string
	steps     []*stepView
	byName    map[string]*stepView
	spin      spinner.Model
	cancel    func()
	canceling bool
	finished  bool
	err       error
	summary   string
	width     int
}

func (m *model) Init() tea.Cmd { return m.spin.Tick }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" && !m.canceling {
			m.canceling = true
			m.cancel()
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case eventMsg:
		m.apply(install.Event(msg))
	case finishMsg:
		m.finished, m.err, m.summary = true, msg.err, msg.summary
		return m, tea.Quit
	}
	return m, nil
}

func (m *model) apply(e install.Event) {
	if e.Kind == install.Note {
		m.notes = append(m.notes, e.Msg)
		return
	}
	s := m.byName[e.Step]
	if s == nil {
		s = &stepView{name: e.Step}
		m.steps = append(m.steps, s)
		m.byName[e.Step] = s
	}
	switch e.Kind {
	case install.StepStarted:
		s.state, s.started = running, time.Now()
	case install.StepProgress:
		s.progress = append(s.progress, e.Msg)
		if len(s.progress) > progressLines {
			s.progress = s.progress[len(s.progress)-progressLines:]
		}
	case install.StepDone:
		s.state, s.msg, s.elapsed, s.progress = done, e.Msg, e.Elapsed, nil
	case install.StepFailed:
		s.state, s.elapsed = failed, e.Elapsed
	}
}

func (m *model) View() string {
	var b strings.Builder
	b.WriteString(titleSt.Render("◆ Agent Substrate") + dimSt.Render(" · "+m.title) + "\n")
	for _, n := range m.notes {
		b.WriteString(warnSt.Render("! "+n) + "\n")
	}
	b.WriteString("\n")
	for _, s := range m.steps {
		switch s.state {
		case pending:
			b.WriteString(dimSt.Render("○ "+s.name) + "\n")
		case running:
			fmt.Fprintf(&b, "%s %s %s\n", m.spin.View(), s.name, dimSt.Render(elapsed(time.Since(s.started))))
			for _, p := range s.progress {
				b.WriteString(dimSt.Render("    "+p) + "\n")
			}
		case done:
			line := okSt.Render("✓") + " " + s.name
			if s.msg != "" {
				line += dimSt.Render(": " + s.msg)
			}
			b.WriteString(line + " " + dimSt.Render(elapsed(s.elapsed)) + "\n")
		case failed:
			fmt.Fprintf(&b, "%s %s %s\n", errSt.Render("✗"), s.name, dimSt.Render(elapsed(s.elapsed)))
		}
	}
	box := boxSt
	if m.width > 4 {
		// Wrap long text to the terminal, less the border.
		box = box.MaxWidth(m.width).Width(min(m.width-2, lipgloss.Width(m.summary+errText(m.err))+2))
	}
	switch {
	case m.finished && m.err != nil:
		b.WriteString(box.BorderForeground(red).Render(errSt.Render(m.err.Error())) + "\n")
	case m.finished && m.summary != "":
		b.WriteString(box.BorderForeground(green).Render(m.summary) + "\n")
	case m.canceling:
		b.WriteString(warnSt.Render("\ncanceling…") + "\n")
	}
	return b.String()
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func elapsed(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

// Reporter renders events. It starts drawing at the Plan event, so a prompt
// that runs before the first step still owns the terminal.
type Reporter struct {
	in     io.Reader
	w      io.Writer
	model  *model
	prog   *tea.Program
	exited chan struct{}
}

// New returns a Reporter that reads keys from in and draws to w. cancel is
// called on Ctrl+C.
func New(in io.Reader, w io.Writer, title string, cancel func()) *Reporter {
	sp := spinner.New(spinner.WithSpinner(spinner.Dot), spinner.WithStyle(lipgloss.NewStyle().Foreground(accent)))
	return &Reporter{
		in:    in,
		w:     w,
		model: &model{title: title, byName: map[string]*stepView{}, spin: sp, cancel: cancel},
	}
}

// Report implements install.Reporter.
func (r *Reporter) Report(e install.Event) {
	if r.prog == nil {
		if e.Kind != install.Plan {
			r.model.apply(e)
			return
		}
		for _, name := range e.Steps {
			r.model.apply(install.Event{Step: name, Kind: install.Plan})
		}
		// The caller handles SIGINT; Ctrl+C on the terminal arrives as a key.
		r.prog = tea.NewProgram(r.model, tea.WithInput(r.in), tea.WithOutput(r.w), tea.WithoutSignalHandler())
		r.exited = make(chan struct{})
		go func() {
			defer close(r.exited)
			// Run fails only on terminal errors; the final view is lost then
			// and Close returns the install error.
			_, _ = r.prog.Run()
		}()
		return
	}
	r.prog.Send(eventMsg(e))
}

// Close draws the final view, with err or summary in a box, and stops
// drawing. It reports whether the view showed err. Without a Plan event
// nothing was drawn, so Close prints only the notes.
func (r *Reporter) Close(err error, summary string) bool {
	if r.prog == nil {
		for _, n := range r.model.notes {
			fmt.Fprintln(r.w, warnSt.Render("! "+n))
		}
		return false
	}
	r.prog.Send(finishMsg{err: err, summary: summary})
	<-r.exited
	return r.model.finished
}
