package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ModelMapper resolves a requested model to the model actually sent upstream.
// Returning an empty target means "no mapping configured".
type ModelMapper func(source string) (target string, err error)

// rewriteModel applies the configured alias to the request body. It returns the
// body to forward and the model the upstream will receive. A missing mapping
// passes the body through untouched so unconfigured models keep working.
func rewriteModel(raw []byte, requested string, lookup ModelMapper) ([]byte, string, error) {
	if lookup == nil || requested == "" {
		return raw, requested, nil
	}
	target, err := lookup(requested)
	if err != nil || target == "" || target == requested {
		// A lookup failure must never break a request: fall back to passthrough.
		return raw, requested, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return raw, requested, nil
	}
	doc["model"] = target
	out, err := json.Marshal(doc)
	if err != nil {
		return raw, requested, fmt.Errorf("encode mapped request: %w", err)
	}
	return out, target, nil
}

// modelFromRaw extracts the requested model without touching other fields.
func modelFromRaw(raw []byte) string {
	var doc struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(raw, &doc)
	return strings.TrimSpace(doc.Model)
}
