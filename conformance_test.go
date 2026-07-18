package fluid_test

// Specs for CLO-210 "Shared conformance fixtures and scenario manifest".
// The same fixtures drive unit validation here and harness E2E runs
// (CLO-212/213/214) unchanged.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cloud-Byte-Consulting/fluid/jsonschema"
	"github.com/Cloud-Byte-Consulting/fluid/spec"
)

type manifest struct {
	Revision  int `json:"revision"`
	Scenarios []struct {
		ID      string `json:"id"`
		Gherkin string `json:"gherkin"`
		Fixture string `json:"fixture"`
		Expect  string `json:"expect"` // "valid" or a required error substring
	} `json:"scenarios"`
}

func loadManifest(t *testing.T) manifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "conformance", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Scenario: Fixtures validate — positive fixtures pass, each negative fixture
// fails for its intended reason.
func TestConformance_FixturesMatchManifest(t *testing.T) {
	m := loadManifest(t)
	if len(m.Scenarios) == 0 {
		t.Fatal("empty manifest")
	}
	for _, sc := range m.Scenarios {
		t.Run(sc.ID, func(t *testing.T) {
			doc, err := os.ReadFile(filepath.Join("testdata", "conformance", sc.Fixture))
			if err != nil {
				t.Fatal(err)
			}
			_, err = spec.Parse(doc)
			if sc.Expect == "valid" {
				if err != nil {
					t.Fatalf("%s: want valid, got %v", sc.Fixture, err)
				}
				// Valid fixtures must also pass the published JSON Schema.
				if v := jsonschema.Validate(spec.Schema(), doc); len(v) != 0 {
					t.Fatalf("%s: schema rejected: %v", sc.Fixture, v)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s: want error containing %q, got none", sc.Fixture, sc.Expect)
			}
			if !strings.Contains(err.Error(), sc.Expect) {
				t.Fatalf("%s: error %q missing %q", sc.Fixture, err, sc.Expect)
			}
		})
	}
}

// Scenario: Language neutrality — fixtures are plain JSON.
func TestConformance_FixturesAreLanguageNeutral(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("testdata", "conformance"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 7 {
		t.Fatalf("expected fixture set, got %d files", len(entries))
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			t.Fatalf("non-JSON artifact in fixture set: %s", e.Name())
		}
		data, err := os.ReadFile(filepath.Join("testdata", "conformance", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !json.Valid(data) {
			t.Fatalf("%s is not valid JSON", e.Name())
		}
	}
}
