package fingerprint

import (
	"math"
	"strings"
	"testing"
)

func TestFingerprintIsDeterministic(t *testing.T) {
	args1 := map[string]any{"command": "npm test", "description": "run tests"}
	args2 := map[string]any{"description": "run tests", "command": "npm test"}

	_, h1, err := Compute("Bash", args1)
	if err != nil {
		t.Fatal(err)
	}
	_, h2, err := Compute("Bash", args2)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("hashes differ across key order:\n %q vs\n %q", h1, h2)
	}
}

func TestFingerprintBashWhitespace(t *testing.T) {
	args1 := map[string]any{"command": "npm test"}
	args3 := map[string]any{"command": "  npm test  "}

	hashes := []string{}
	for _, a := range []map[string]any{args1, args3} {
		_, h, err := Compute("Bash", a)
		if err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, h)
	}
	for i := 1; i < len(hashes); i++ {
		if hashes[i] != hashes[0] {
			t.Fatalf("Bash whitespace not normalized: %q != %q at idx %d", hashes[0], hashes[i], i)
		}
	}
}

func TestFingerprintBashPreservesInternalWhitespace(t *testing.T) {
	_, h1, err := Compute("Bash", map[string]any{"command": `grep "a  b" file.txt`})
	if err != nil {
		t.Fatal(err)
	}
	_, h2, err := Compute("Bash", map[string]any{"command": `grep "a b" file.txt`})
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Fatal("Bash fingerprint must preserve meaningful internal whitespace")
	}

	_, h3, err := Compute("Bash", map[string]any{"command": "npm  test"})
	if err != nil {
		t.Fatal(err)
	}
	_, h4, err := Compute("Bash", map[string]any{"command": "npm test"})
	if err != nil {
		t.Fatal(err)
	}
	if h3 == h4 {
		t.Fatal("Bash fingerprint must not collapse internal token spacing")
	}
}

func TestFingerprintDistinctArgs(t *testing.T) {
	_, h1, _ := Compute("Bash", map[string]any{"command": "npm test"})
	_, h2, _ := Compute("Bash", map[string]any{"command": "npm run test"})
	if h1 == h2 {
		t.Fatalf("byte-different commands should not collide: %q", h1)
	}
}

func TestFingerprintDistinctTools(t *testing.T) {
	_, h1, _ := Compute("Bash", map[string]any{"command": "x"})
	_, h2, _ := Compute("Read", map[string]any{"command": "x"})
	if h1 == h2 {
		t.Fatalf("different tools should not collide")
	}
}

func TestFingerprintIgnoresEmpty(t *testing.T) {
	_, h1, _ := Compute("Bash", map[string]any{"command": "ls"})
	_, h2, _ := Compute("Bash", map[string]any{"command": "ls", "description": ""})
	_, h3, _ := Compute("Bash", map[string]any{"command": "ls", "description": nil, "timeout": nil})
	if h1 != h2 || h1 != h3 {
		t.Fatalf("empty/nil fields should be ignored: %q %q %q", h1, h2, h3)
	}
}

func TestFingerprintNestedObjectsAndArrays(t *testing.T) {
	args1 := map[string]any{
		"file_path": "/tmp/a.go",
		"edits": []any{
			map[string]any{"new": "b", "old": "a", "empty": ""},
			map[string]any{},
			nil,
		},
		"options": map[string]any{
			"limit": float64(20),
			"tags":  []any{"x", "", "y"},
		},
	}
	args2 := map[string]any{
		"options": map[string]any{
			"tags":  []any{"x", "y"},
			"limit": float64(20),
		},
		"edits": []any{
			map[string]any{"old": "a", "new": "b"},
		},
		"file_path": "/tmp/a.go",
	}
	_, h1, err := Compute("Edit", args1)
	if err != nil {
		t.Fatal(err)
	}
	_, h2, err := Compute("Edit", args2)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("nested normalization should be deterministic: %q != %q", h1, h2)
	}
}

func TestComputeReturnsMarshalErrorForUnsupportedValues(t *testing.T) {
	_, _, err := Compute("Bash", map[string]any{"command": "npm test", "nan": math.NaN()})
	if err == nil {
		t.Fatal("expected marshal error for NaN")
	}
	if !strings.Contains(err.Error(), "fingerprint: marshal") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestComputeNilArgsCanonicalizesToNull(t *testing.T) {
	canonical, _, err := Compute("Read", nil)
	if err != nil {
		t.Fatal(err)
	}
	if canonical != `{"args":null,"tool":"Read"}` {
		t.Fatalf("canonical = %q", canonical)
	}
}
