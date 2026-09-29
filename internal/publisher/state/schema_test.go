package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	stateSchemaURL   = "https://raw.githubusercontent.com/ovineko/schepherd/main/api/state.schema.json"
	catalogSchemaURL = "https://raw.githubusercontent.com/ovineko/schepherd/main/api/catalog.schema.json"
)

var apiDir = filepath.Join("..", "..", "..", "api")

type noLoader struct{}

func (noLoader) Load(url string) (any, error) {
	return nil, errors.New("the state schema must not load anything but api/catalog.schema.json: " + url)
}

func compileStateSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()

	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(noLoader{})

	for url, file := range map[string]string{stateSchemaURL: "state.schema.json", catalogSchemaURL: "catalog.schema.json"} {
		data, err := os.ReadFile(filepath.Join(apiDir, file))
		if err != nil {
			t.Fatal(err)
		}

		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("decode %s: %v", file, err)
		}

		if err := compiler.AddResource(url, doc); err != nil {
			t.Fatalf("add %s: %v", file, err)
		}
	}

	schema, err := compiler.Compile(stateSchemaURL)
	if err != nil {
		t.Fatalf("api/state.schema.json does not compile: %v", err)
	}

	return schema
}

func validateAgainstSchema(schema *jsonschema.Schema, data []byte) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return err
	}

	return schema.Validate(inst)
}

func TestStateSchemaDocument(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(apiDir, "state.schema.json"))
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		ID     string `json:"$id"`
		Schema string `json:"$schema"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}

	if doc.ID != stateSchemaURL || doc.Schema != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("$id = %q, $schema = %q", doc.ID, doc.Schema)
	}

	compileStateSchema(t)
}

func TestStateSchemaAcceptsEncodedStates(t *testing.T) {
	schema := compileStateSchema(t)

	local := sample()
	local.Source = Source{Kind: KindLocal, Name: "fixtures"}

	empty := sample()
	empty.Schemas = nil

	for name, s := range map[string]*State{"schemastore": sample(), "local": local, "empty": empty} {
		data := mustEncode(t, s)

		if err := validateAgainstSchema(schema, data); err != nil {
			t.Errorf("%s: api/state.schema.json rejects Encode output: %v", name, err)
		}
	}
}

// TestStateSchemaAgreesOnRejections runs every invalid document whose rule
// JSON Schema can express through both validators.
func TestStateSchemaAgreesOnRejections(t *testing.T) {
	schema := compileStateSchema(t)
	checked := 0

	for _, tc := range invalidCases(t) {
		if !tc.schema {
			continue
		}

		checked++

		if _, err := Parse(tc.doc); err == nil {
			t.Errorf("%s: Parse accepts the document", tc.name)
		}

		if err := validateAgainstSchema(schema, tc.doc); err == nil {
			t.Errorf("%s: api/state.schema.json accepts the document", tc.name)
		}
	}

	if checked < 25 {
		t.Fatalf("only %d invalid documents are checked against the schema", checked)
	}
}

// TestStateSchemaNeverStricterThanParse varies the values the schema
// constrains with patterns: whatever Parse accepts, the schema must accept.
func TestStateSchemaNeverStricterThanParse(t *testing.T) {
	schema := compileStateSchema(t)
	accepted := 0

	variants := map[string]func(s *State, v string){
		"repository": func(s *State, v string) { s.Source.Repository = "https://" + v + "/x" },
		"repository path": func(s *State, v string) {
			s.Source.Repository = "https://github.com/" + v
		},
		"recipe":     func(s *State, v string) { s.Recipe = "r" + v + "r" },
		"local name": func(s *State, v string) { s.Source = Source{Kind: KindLocal, Name: "n" + v + "n"} },
		"redirect url": func(s *State, v string) {
			s.Schemas[0].License.Redirects[0].URL = "https://github.com/" + v + "/x"
		},
		"redirect target host": func(s *State, v string) {
			s.Schemas[0].License.Redirects[0].Target = "http://" + v + "/x"
		},
	}

	values := []string{"", " ", "a b", "@", "%41", "[::1]", ":443", "~", "\"", "\\", "é", " ", "\t", "/", "a/b", ".", "*", "{}", "|"}
	for r := rune(0x20); r < 0x7f; r++ {
		values = append(values, string(r))
	}

	for name, change := range variants {
		for _, v := range values {
			s := sample()
			change(s, v)

			data, err := Encode(s)
			if err != nil {
				continue
			}

			accepted++

			if err := validateAgainstSchema(schema, data); err != nil {
				t.Errorf("%s %q: Parse accepts, api/state.schema.json rejects: %v", name, v, err)
			}
		}
	}

	if accepted < 100 {
		t.Fatalf("only %d variants were accepted; the test exercises too little", accepted)
	}
}
