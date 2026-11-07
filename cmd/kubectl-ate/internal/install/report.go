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

package install

import (
	"fmt"
	"io"
	"time"
)

// EventKind says what happened to a step.
type EventKind int

const (
	// StepStarted is sent once before a step runs.
	StepStarted EventKind = iota
	// StepProgress carries a status line while a step runs.
	StepProgress
	// StepDone is sent when a step succeeds. Msg summarizes what it did.
	StepDone
	// StepFailed is sent when a step fails. Err holds the cause.
	StepFailed
	// Note is a message outside any step, such as a warning.
	Note
	// Plan is sent once before the first step. Steps lists every step name.
	Plan
)

// Event is one thing the installer reports. Renderers turn events into
// terminal output.
type Event struct {
	Kind    EventKind
	Step    string
	Msg     string
	Err     error
	Elapsed time.Duration
	Steps   []string
}

// Reporter receives events. Report is called from one goroutine at a time.
type Reporter interface {
	Report(Event)
}

// PlainReporter writes one line per event. It works in logs and on terminals
// that can't redraw.
type PlainReporter struct {
	W io.Writer
}

// Report implements Reporter.
func (p PlainReporter) Report(e Event) {
	switch e.Kind {
	case StepStarted:
		fmt.Fprintf(p.W, "• %s\n", e.Step)
	case StepProgress:
		fmt.Fprintf(p.W, "    %s\n", e.Msg)
	case StepDone:
		if e.Msg != "" {
			fmt.Fprintf(p.W, "✓ %s: %s (%s)\n", e.Step, e.Msg, roundElapsed(e.Elapsed))
		} else {
			fmt.Fprintf(p.W, "✓ %s (%s)\n", e.Step, roundElapsed(e.Elapsed))
		}
	case StepFailed:
		fmt.Fprintf(p.W, "✗ %s (%s)\n", e.Step, roundElapsed(e.Elapsed))
	case Note:
		fmt.Fprintf(p.W, "! %s\n", e.Msg)
	}
}

func roundElapsed(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(time.Millisecond)
	}
	return d.Round(100 * time.Millisecond)
}
