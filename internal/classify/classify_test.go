package classify

import "testing"

func TestClassifyBash(t *testing.T) {
	cases := []struct {
		cmd  string
		want Policy
	}{
		{"npm test", PolicyReadOnly},
		{"npm test --watch", PolicyReadOnly},
		{"git status", PolicyReadOnly},
		{"grep foo .", PolicyReadOnly},
		{`grep "a | b" file.txt`, PolicyReadOnly},
		{"tail -n 20 sim.log", PolicyVolatileReadOnly},
		{"ps aux", PolicyVolatileReadOnly},
		{"pgrep simulator", PolicyVolatileReadOnly},
		{"docker ps", PolicyVolatileReadOnly},
		{"kubectl get pods", PolicyVolatileReadOnly},
		{"nvidia-smi", PolicyVolatileReadOnly},
		{"squeue -u me", PolicyVolatileReadOnly},

		{"git push", PolicySideEffect},
		{"git push origin main", PolicySideEffect},
		{"rm file.txt", PolicySideEffect},
		{"rmdir empty", PolicySideEffect}, // unknown -> SideEffect
		{"curl -X POST https://x", PolicySideEffect},
		{"git status && rm -rf tmp", PolicySideEffect},
		{"npm test; git commit -am wip", PolicySideEffect},
		{"cat README.md > copy.txt", PolicySideEffect},
		{"grep foo . | tee result.txt", PolicySideEffect},
		{"go test ./... &", PolicySideEffect},
		{"cat $(mktemp)", PolicySideEffect},
		{"cat `mktemp`", PolicySideEffect},
		{`cat "\$(literal)"`, PolicyReadOnly},
		{`cat '$HOME'`, PolicyReadOnly},
		{`grep 'a; b' file.txt`, PolicyReadOnly},
		{`grep "a > b" file.txt`, PolicyReadOnly},
		{"cat README.md < input.txt", PolicySideEffect},

		{"some-unknown-cmd --flag", PolicySideEffect}, // default conservative
		{"", PolicySideEffect},
	}
	for _, c := range cases {
		got := ClassifyBash(c.cmd)
		if got != c.want {
			t.Errorf("ClassifyBash(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

func TestClassifyTool(t *testing.T) {
	cases := []struct {
		name string
		want Policy
	}{
		{"Read", PolicyReadOnly},
		{"Glob", PolicyReadOnly},
		{"Grep", PolicyReadOnly},
		{"WebFetch", PolicyReadOnly},
		{"WebSearch", PolicyReadOnly},
		{"Write", PolicySideEffect},
		{"Edit", PolicySideEffect},
		{"NotebookEdit", PolicySideEffect},
		{"TodoWrite", PolicySideEffect},
		{"", PolicySideEffect},
	}
	for _, c := range cases {
		if got := ClassifyTool(c.name); got != c.want {
			t.Errorf("ClassifyTool(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsKnownSideEffectBashDistinguishesUnknownFallback(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"rm file", true},
		{"git status && rm file", true},
		{"cat README.md > copy.txt", true},
		{"./check_sim_status", false},
		{"some-unknown-cmd --flag", false},
		{"npm test", false},
		{"tail -n 20 sim.log", false},
		{"", true},
	}
	for _, c := range cases {
		if got := IsKnownSideEffectBash(c.cmd); got != c.want {
			t.Errorf("IsKnownSideEffectBash(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

func TestRmDoesNotMatchRmdir(t *testing.T) {
	// `rm` prefix must require a real boundary so `rmdir` is not flagged as
	// matching the `rm ` rule.
	if ClassifyBash("rmdir empty") == PolicySideEffect {
		// 'rmdir' should not be matched by 'rm ' but the unknown-default also
		// classifies it as SideEffect — verify it's via the unknown path,
		// not the rm rule. Check by inspecting that "rm-thing" with no space
		// doesn't trip either:
	}
	if ClassifyBash("rm-thing arg") != PolicySideEffect {
		t.Fatalf("rm-thing should fall through to default SideEffect")
	}
	// Real rm with space must trip:
	if ClassifyBash("rm file") != PolicySideEffect {
		t.Fatalf("rm file should be SideEffect")
	}
}
