package codex

import "encoding/json"

// decodeJSONShim is a tiny wrapper kept in a separate file so test imports
// don't grow encoding/json directly. Lets us keep the cross-package test
// readable without polluting the main test file's import list.
func decodeJSONShim(b []byte, v any) error {
	return json.Unmarshal(b, v)
}
