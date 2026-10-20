package metaschema

import "testing"

func TestIsStandard(t *testing.T) {
	valid := []string{
		"http://json-schema.org/draft-04/schema",
		"http://json-schema.org/draft-04/schema#",
		"https://json-schema.org/draft-06/schema#",
		"http://json-schema.org/draft-07/schema#",
		"https://json-schema.org/draft/2019-09/schema",
		"https://json-schema.org/draft/2020-12/schema",
		"https://JSON-Schema.org/draft/2020-12/schema#",
	}

	invalid := []string{
		"http://json-schema.org/draft-03/schema#",
		"http://json-schema.org/draft-07/schema#/definitions/x",
		"http://json-schema.org/draft-07/schema?x",
		"http://json-schema.org/draft-07/schema/",
		"http://json-schema.org:80/draft-07/schema",
		"http://user@json-schema.org/draft-07/schema",
		"ftp://json-schema.org/draft-07/schema",
		"https://json-schema.org.evil.example/draft-07/schema",
		"https://json-schema.org/draft/2020-12/meta/core",
		"https://json-schema.org/draft/2020-12/schema#/$defs",
		"",
		"%",
	}

	for _, uri := range valid {
		if !IsStandard(uri) {
			t.Errorf("IsStandard(%q) = false", uri)
		}
	}

	for _, uri := range invalid {
		if IsStandard(uri) {
			t.Errorf("IsStandard(%q) = true", uri)
		}
	}
}
