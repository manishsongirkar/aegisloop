package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"aegisloop/internal/config"
)

// Every ```yaml block in the README must be a configuration AegisLoop accepts,
// so the documentation can never drift from the parser.
func TestReadmeYAMLExamplesAreValid(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	blocks := regexp.MustCompile("(?s)```yaml\n(.*?)```").FindAllStringSubmatch(string(b), -1)
	if len(blocks) < 3 {
		t.Fatalf("expected at least 3 YAML examples, found %d", len(blocks))
	}
	for i, m := range blocks {
		if _, err := config.Parse(m[1]); err != nil {
			t.Errorf("README YAML example #%d is invalid: %v\n%s", i+1, err, strings.TrimSpace(m[1]))
		}
	}
}
