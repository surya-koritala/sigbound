// -config support for `sig run`: a flat KEY=VALUE flags file supplying
// defaults for the other run flags, so the standing invocation for a project
// doesn't have to be retyped every time (see issue #13). Despite the issue's
// working title ("sig.toml"), the format is NOT TOML: sigbound is stdlib-only
// (no new module dependencies), and the standard library has no TOML parser.
// A flat "one flag per line" file needs no parser dependency at all and keeps
// every value exactly what it would be on the command line — no quoting
// rules to learn. See docs/USAGE.md's "Config file" section for the format
// and a full example.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// configDiscoveryName is the file -config looks for when the flag is left at
// its default (unset) value — see resolveConfigPath.
const configDiscoveryName = "sig.conf"

// configDisableSentinel is the documented value that turns OFF -config
// discovery entirely. It exists because "-config" with an explicit empty
// value ("-config=") is indistinguishable from not passing -config at all —
// both leave the flag's string value at "" — so there is no way to tell
// "explicitly disabled" from "unset" without a dedicated sentinel.
const configDisableSentinel = "none"

// configEntry is one KEY=VALUE line from a -config file, carrying its 1-based
// line number so a bad key or value can be reported with exactly where it
// came from.
type configEntry struct {
	Key   string
	Value string
	Line  int
}

// resolveConfigPath implements -config's three-way behavior:
//
//   - configDisableSentinel ("none"): config is off; no file is read.
//   - "" (the flag's default, i.e. -config was not passed): look for
//     configDiscoveryName in the CURRENT WORKING DIRECTORY ONLY — no home-dir
//     fallback, no walking up toward a repo root. Finding nothing is not an
//     error; it just means no config file applies to this run.
//   - anything else: an explicit path. It must exist and be readable, since
//     the caller asked for it by name.
//
// Returns "" (no error) when no config file should be applied.
func resolveConfigPath(configFlag string) (string, error) {
	switch configFlag {
	case configDisableSentinel:
		return "", nil
	case "":
		if _, err := os.Stat(configDiscoveryName); err != nil {
			return "", nil // no sig.conf here; discovery is best-effort
		}
		return configDiscoveryName, nil
	default:
		return configFlag, nil
	}
}

// parseConfigFile parses a flat KEY=VALUE flags file: one flag per line, key
// the flag's name without its leading dash, value exactly what would follow
// it on the command line. A line is a comment (ignored) when its first
// non-whitespace character is '#'; blank lines are also ignored. CRLF line
// endings are tolerated (a trailing '\r' is stripped before parsing). The
// key/value split is on the FIRST '=' only, so a value may itself contain
// '=' (e.g. verify=FOO=bar && go build ./...); both key and value are
// whitespace-trimmed at their edges, but interior spacing in the value is
// preserved exactly. A line with no '=' at all is a malformed-line error
// naming its 1-based line number, likewise a line whose key is empty
// (e.g. "=value").
func parseConfigFile(data []byte) ([]configEntry, error) {
	var entries []configEntry
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSuffix(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE, got %q", i+1, trimmed)
		}
		key := strings.TrimSpace(line[:eq])
		if key == "" {
			return nil, fmt.Errorf("line %d: empty key", i+1)
		}
		value := strings.TrimSpace(line[eq+1:])
		entries = append(entries, configEntry{Key: key, Value: value, Line: i + 1})
	}
	return entries, nil
}

// applyConfigFile resolves -config (discovery, the "none" sentinel, or an
// explicit path — see resolveConfigPath) and, for every KEY=VALUE line whose
// key was NOT already set explicitly on the command line, applies it to fs
// via fs.Set — reusing flag.Value's own parsing/validation for every flag
// type (bool, int, Duration, ...) instead of reimplementing it here. This is
// what gives -config its precedence: command-line flag > config file > flag
// default, since fs.Parse already applied every explicit command-line value
// before this runs, and applyConfigFile only ever touches flags that weren't
// already given explicitly.
//
// "config" itself is never allowed as a key inside the file — accepting it
// would be self-referential (and silently ignored either way, since the file
// is already resolved by the time this reads it) — so it is rejected with a
// clear, line-numbered error instead of being quietly permitted or ignored.
//
// explicit is both an input and an output: on input it holds every flag name
// fs.Visit reported as set on the command line; every key this function DOES
// apply from the config file is added to it too, so callers that ask "was
// this flag explicitly chosen" downstream (e.g. runRun's -lanes
// strict-default logic) see a config-file choice the same way they'd see a
// command-line one. A config value is still a deliberate choice, even though
// it didn't arrive via argv.
func applyConfigFile(fs *flag.FlagSet, configFlag string, explicit map[string]bool) error {
	path, err := resolveConfigPath(configFlag)
	if err != nil {
		return err
	}
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("-config %s: %w", path, err)
	}
	entries, err := parseConfigFile(data)
	if err != nil {
		return fmt.Errorf("-config %s: %w", path, err)
	}
	for _, e := range entries {
		if e.Key == "config" {
			return fmt.Errorf("-config %s:%d: %q is not allowed inside a config file", path, e.Line, e.Key)
		}
		if explicit[e.Key] {
			continue // an explicit command-line flag always wins over the config file
		}
		if err := fs.Set(e.Key, e.Value); err != nil {
			return fmt.Errorf("-config %s:%d: %w", path, e.Line, err)
		}
		explicit[e.Key] = true
	}
	return nil
}
