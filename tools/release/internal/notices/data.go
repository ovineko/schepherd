package notices

import (
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/upstream/tomlfile"
)

// Repository files the generator reads besides the Go modules and the state.
const (
	DataFile   = "tools/release/notices.toml"
	PolicyFile = "sources/licenses.toml"
)

const maxInputBytes = 1 << 20

// data is DataFile: what the notices need to know about the catalog's
// sources that no manifest records.
type data struct {
	// SchemaStore names the policy rules that allow files of the SchemaStore
	// repository and quotes that repository's NOTICE file.
	SchemaStore schemaStoreData `toml:"schemastore"`
}

type schemaStoreData struct {
	Notice string   `toml:"notice"`
	Rules  []string `toml:"rules"`
}

func loadData(root string) (*data, error) {
	var d data
	if err := tomlfile.Decode(filepath.Join(root, filepath.FromSlash(DataFile)), maxInputBytes, &d); err != nil {
		return nil, err //nolint:wrapcheck // already fault.Usage naming the file
	}

	d.SchemaStore.Notice = strings.ReplaceAll(d.SchemaStore.Notice, "\r\n", "\n")

	if len(d.SchemaStore.Rules) == 0 || strings.TrimSpace(d.SchemaStore.Notice) == "" {
		return nil, fault.New(fault.Usage, "%s: schemastore needs rules and notice", DataFile)
	}

	return &d, nil
}

// policyRule is what the notices show of a rule of PolicyFile, which the
// publisher validates in full.
type policyRule struct {
	ID       string `toml:"id"`
	Decision string `toml:"decision"`
	License  string `toml:"license"`
	Reason   string `toml:"reason"`
}

func loadRules(root string) (map[string]policyRule, error) {
	content, err := tomlfile.Read(filepath.Join(root, filepath.FromSlash(PolicyFile)), maxInputBytes)
	if err != nil {
		return nil, err //nolint:wrapcheck // already fault.Usage naming the file
	}

	var doc struct {
		Rules []policyRule `toml:"rules"`
	}

	if err := toml.Unmarshal(content, &doc); err != nil {
		return nil, fault.Wrap(fault.Usage, err, "%s", PolicyFile)
	}

	rules := make(map[string]policyRule, len(doc.Rules))
	for _, r := range doc.Rules {
		rules[r.ID] = r
	}

	return rules, nil
}
