package codex

import (
	"encoding/json"
	"strconv"
	"strings"
)

// JSON helpers for parsing Codex session lines. All are whitespace- and
// string-numeric tolerant so partial or loosely-typed records still parse.

// typeOf extracts a "type" field value, tolerating surrounding whitespace.
func typeOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// objOf decodes a raw message into a string-keyed object, or nil.
func objOf(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// modelFrom pulls a model id from a payload object via the model chain.
func modelFrom(raw json.RawMessage) string {
	return firstModel(objOf(raw))
}

// firstModel scans the given objects for model id under the standard chain
// keys (model, model_name, metadata.model), returning the first non-empty.
func firstModel(objs ...map[string]json.RawMessage) string {
	for _, o := range objs {
		if o == nil {
			continue
		}
		for _, k := range []string{"model", "model_name"} {
			if s := strOf(o[k]); s != "" {
				return s
			}
		}
		if meta := objOf(o["metadata"]); meta != nil {
			if s := strOf(meta["model"]); s != "" {
				return s
			}
		}
	}
	return ""
}

// strOf decodes a raw message as a trimmed string, or "".
func strOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// readRaw extracts the token aliases from a usage object.
func readRaw(o map[string]json.RawMessage) rawTokens {
	return rawTokens{
		input:     numAlias(o, "input_tokens", "prompt_tokens", "input"),
		cached:    numAlias(o, "cached_input_tokens", "cache_read_input_tokens", "cached_tokens"),
		output:    numAlias(o, "output_tokens", "completion_tokens", "output"),
		reasoning: numAlias(o, "reasoning_output_tokens", "reasoning_tokens"),
		total:     numAlias(o, "total_tokens"),
	}
}

// numAlias returns the first present numeric value among keys, tolerating
// numeric strings with surrounding whitespace.
func numAlias(o map[string]json.RawMessage, keys ...string) int64 {
	for _, k := range keys {
		if raw, ok := o[k]; ok {
			if v, ok := asInt(raw); ok {
				return v
			}
		}
	}
	return 0
}

// asInt parses a JSON number or numeric string into int64.
func asInt(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	// Try a JSON number first.
	var num json.Number
	if err := json.Unmarshal(raw, &num); err == nil {
		if i, err := num.Int64(); err == nil {
			return i, true
		}
		if f, err := num.Float64(); err == nil {
			return int64(f), true
		}
	}
	// Then a quoted numeric string with whitespace.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return 0, false
		}
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i, true
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return int64(f), true
		}
	}
	return 0, false
}
