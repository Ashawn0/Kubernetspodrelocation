package logevent

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Record is one Stage 0 JSONL line.
type Record struct {
	Split     string         `json:"split"`
	Probe     string         `json:"probe"`
	Check     string         `json:"check"`
	Pass      bool           `json:"pass"`
	Timestamp time.Time      `json:"ts"`
	Detail    map[string]any `json:"detail,omitempty"`
	Error     string         `json:"error,omitempty"`
}

// Writer appends JSONL records.
type Writer struct {
	f *os.File
}

func Create(path string) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Writer{f: f}, nil
}

func (w *Writer) Close() error {
	if w == nil || w.f == nil {
		return nil
	}
	return w.f.Close()
}

func (w *Writer) Write(r Record) error {
	if r.Split == "" {
		r.Split = "stage0"
	}
	if r.Timestamp.IsZero() {
		r.Timestamp = time.Now().UTC()
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w.f, "%s\n", b)
	return err
}
