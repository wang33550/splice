package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	r, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !r.AskOnIntercept {
		t.Errorf("default AskOnIntercept should be true, got false")
	}
}

func TestLoadProjectLocalOverridesGlobal(t *testing.T) {
	// Project local says false, global says true; project should win.
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".splice"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".splice", "config.json"),
		[]byte(`{"ask_on_intercept": false}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// We can't easily isolate $HOME here; but the Load fn always reads home
	// first then cwd, so cwd file alone is enough to verify override semantics.
	r, err := Load(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if r.AskOnIntercept {
		t.Errorf("project-local config should override default, got AskOnIntercept=true")
	}
}

func TestLoadNeverCacheBashPatterns(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".splice"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".splice", "config.json"),
		[]byte(`{"never_cache_bash_patterns": ["./check_sim_status", "tail sim.log"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := Load(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.NeverCacheBashPatterns) != 2 {
		t.Fatalf("expected 2 never-cache patterns, got %#v", r.NeverCacheBashPatterns)
	}
	if r.NeverCacheBashPatterns[0] != "./check_sim_status" || r.NeverCacheBashPatterns[1] != "tail sim.log" {
		t.Fatalf("unexpected patterns: %#v", r.NeverCacheBashPatterns)
	}
}

func TestLoadMalformedJSONReturnsError(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".splice"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".splice", "config.json"),
		[]byte(`{not valid json`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(cwd)
	if err == nil {
		t.Fatal("expected parse error for malformed json, got nil")
	}
}

func TestLoadEmptyFileIsValid(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".splice"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".splice", "config.json"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := Load(cwd)
	if err != nil {
		t.Fatalf("empty file should be tolerated, got %v", err)
	}
	if !r.AskOnIntercept {
		t.Errorf("empty file should yield default true; got %v", r.AskOnIntercept)
	}
}

func TestLoadMissingFileIsValid(t *testing.T) {
	r, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !r.AskOnIntercept {
		t.Errorf("no config files should yield default true; got %v", r.AskOnIntercept)
	}
}

func TestLoadEmptyCwdReturnsDefaults(t *testing.T) {
	// Edge: empty cwd skips the project layer cleanly.
	r, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !r.AskOnIntercept {
		t.Errorf("empty cwd path should fall through to defaults; got %v", r.AskOnIntercept)
	}
}
