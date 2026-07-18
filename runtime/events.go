package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// event is one journal line. The journal is the single source of truth for
// run state: replaying it reconstructs results (for resume) and status.
type event struct {
	T       time.Time       `json:"t"`
	Event   string          `json:"event"` // run_started | node_started | node_done | node_failed | run_done | run_failed | run_cancelled
	Node    string          `json:"node,omitempty"`
	Attempt int             `json:"attempt,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Tokens  int             `json:"tokens,omitempty"`
	Error   string          `json:"error,omitempty"`
	Reason  string          `json:"reason,omitempty"`
	Name    string          `json:"name,omitempty"`  // run_started: DAG name
	Total   int             `json:"total,omitempty"` // run_started: node count
}

// appendEvent writes one timestamped event as a single line.
func appendEvent(f *os.File, e event) error {
	e.T = time.Now().UTC()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// readJournal replays a journal file. A truncated final line (crash mid-write)
// is tolerated and ignored; corruption anywhere else is an error. A missing
// file yields (nil, nil).
func readJournal(path string) ([]event, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(data, []byte{'\n'})
	var events []event
	for i, line := range lines {
		if len(line) == 0 {
			continue
		}
		var e event
		if err := json.Unmarshal(line, &e); err != nil {
			if i == len(lines)-1 {
				break // trailing partial line from an interrupted write
			}
			return nil, fmt.Errorf("corrupt journal %s line %d: %w", path, i+1, err)
		}
		events = append(events, e)
	}
	return events, nil
}

func safeMsg(err error) string {
	s := err.Error()
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}
