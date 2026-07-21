// Command sig-testagent is a DETERMINISTIC agent for driving `sig run`
// without a live LLM. It reads its instructions from SIGBOUND_TASK (a small JSON
// spec), applies a fixed file change in its worktree (cwd), and commits — so the
// orchestration driver can be exercised repeatably in tests and benchmarks.
//
// SIGBOUND_TASK JSON spec:
//
//	{
//	  "write": {"path/a.go": "full file content", ...},  // create/overwrite files
//	  "edit":  {"file": "shared.txt", "line": 5, "text": "new line"}  // replace one 0-indexed line
//	}
//
// Either field may be omitted. It commits via the shared hermetic git wrapper so
// the commit identity is fixed and no host gitconfig is consulted.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

type editSpec struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type spec struct {
	Write map[string]string `json:"write"`
	Edit  *editSpec         `json:"edit"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sig-testagent:", err)
		os.Exit(1)
	}
}

func run() error {
	raw := os.Getenv("SIGBOUND_TASK")
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("SIGBOUND_TASK is empty")
	}
	var s spec
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return fmt.Errorf("parse SIGBOUND_TASK JSON: %w", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	// Deterministic file writes (create/overwrite).
	for rel, content := range s.Write {
		full := filepath.Join(cwd, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", rel, err)
		}
	}

	// Deterministic single-line edit of an existing file.
	if s.Edit != nil {
		full := filepath.Join(cwd, filepath.FromSlash(s.Edit.File))
		data, err := os.ReadFile(full)
		if err != nil {
			return fmt.Errorf("read %s for edit: %w", s.Edit.File, err)
		}
		lines := strings.Split(string(data), "\n")
		if s.Edit.Line < 0 || s.Edit.Line >= len(lines) {
			return fmt.Errorf("edit line %d out of range for %s (%d lines)", s.Edit.Line, s.Edit.File, len(lines))
		}
		lines[s.Edit.Line] = s.Edit.Text
		if err := os.WriteFile(full, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
			return fmt.Errorf("write edit %s: %w", s.Edit.File, err)
		}
	}

	// Commit via the shared hermetic wrapper (fixed identity, no host config).
	id := os.Getenv("SIGBOUND_TASK_ID")
	g := gitx.New(cwd)
	if _, err := g.CommitAll(context.Background(), "testagent: "+id); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
