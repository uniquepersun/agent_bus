package bus

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"

	udiff "github.com/aymanbagabas/go-udiff"
	"gopkg.in/yaml.v3"
)

// ApplyEdits applies find/replace edits in order. Each Old must match exactly
// once so an edit can never land somewhere the proposer didn't mean.
func ApplyEdits(content string, edits []Edit) (string, error) {
	for i, e := range edits {
		if e.Old == "" {
			content += e.New
			continue
		}
		switch n := strings.Count(content, e.Old); n {
		case 0:
			return "", fmt.Errorf("edit %d: old text not found in the contract", i)
		case 1:
			content = strings.Replace(content, e.Old, e.New, 1)
		default:
			return "", fmt.Errorf("edit %d: old text appears %d times; include more surrounding lines so it is unique", i, n)
		}
	}
	return content, nil
}

// Validate checks that a contract still parses after edits.
func Validate(contract, content string) error {
	switch path.Ext(contract) {
	case ".yaml", ".yml":
		var v any
		if err := yaml.Unmarshal([]byte(content), &v); err != nil {
			return fmt.Errorf("result is not valid YAML: %v", err)
		}
	case ".json":
		if !json.Valid([]byte(content)) {
			return fmt.Errorf("result is not valid JSON")
		}
	}
	return nil
}

func Diff(contract, before, after string) string {
	return udiff.Unified("a/"+contract, "b/"+contract, before, after)
}
