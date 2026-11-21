package router

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

type object = map[string]any

// sanitize turns a Claude Code request body into one a third-party provider
// can accept: allowlisted top-level keys, no billing block, no Anthropic-only
// constructs, model rewritten. The input is untrusted; malformed shapes are
// errors, never panics.
func sanitize(p Sanitize, model string, body []byte) ([]byte, error) {
	in, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	out := object{}
	for _, k := range p.TopKeys {
		if v, ok := in[k]; ok {
			out[k] = v
		}
	}
	out["model"] = model

	if sys, ok := out["system"]; ok {
		cleaned, err := cleanSystem(sys)
		if err != nil {
			return nil, err
		}
		setOrDelete(out, "system", cleaned)
	}

	msgs, hoisted, err := normalizeMessages(out["messages"], p.BlockTypes)
	if err != nil {
		return nil, err
	}
	out["messages"] = msgs

	tools, err := cleanTools(out["tools"], hoisted)
	if err != nil {
		return nil, err
	}
	setOrDelete(out, "tools", tools)
	fixToolChoice(out, tools)
	fixThinking(out)
	fixOutputConfig(out)

	return encode(out)
}

func decodeObject(body []byte) (object, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // keep numbers verbatim
	var obj object
	if err := dec.Decode(&obj); err != nil {
		return nil, fmt.Errorf("body is not a JSON object: %w", err)
	}
	if obj == nil {
		return nil, errors.New("body is not a JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after JSON body")
	}
	return obj, nil
}

func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// setOrDelete stores v under k, or removes k when v is an empty list.
func setOrDelete(o object, k string, v []any) {
	if len(v) == 0 {
		delete(o, k)
		return
	}
	o[k] = v
}

// cleanSystem drops billing blocks and keeps only text blocks as {type,text}.
func cleanSystem(sys any) ([]any, error) {
	switch s := sys.(type) {
	case string:
		if strings.HasPrefix(s, billingPrefix) {
			return nil, nil
		}
		return []any{object{"type": "text", "text": s}}, nil
	case []any:
		var out []any
		for _, b := range s {
			blk, ok := b.(object)
			if !ok {
				return nil, errors.New("system: block is not an object")
			}
			text, _ := blk["text"].(string)
			if blk["type"] != "text" || strings.HasPrefix(text, billingPrefix) {
				continue
			}
			out = append(out, object{"type": "text", "text": text})
		}
		return out, nil
	default:
		return nil, errors.New("system: must be a string or an array")
	}
}

// normalizeMessages filters content blocks and rewrites role:"system"
// messages: their tool_addition definitions are hoisted into tools, their
// text is attached to the nearest user message (the preceding one, else the
// end of the next one) so tool_result blocks stay first. Adjacent messages
// with the same role are coalesced.
func normalizeMessages(v any, allowed []string) (msgs []any, hoisted []any, err error) {
	list, ok := v.([]any)
	if !ok {
		return nil, nil, errors.New("messages: must be an array")
	}
	var out []object
	var pending []any // system-derived text waiting for a user message
	push := func(role string, blocks []any) {
		if n := len(out); n > 0 && out[n-1]["role"] == role {
			out[n-1]["content"] = append(out[n-1]["content"].([]any), blocks...)
			return
		}
		out = append(out, object{"role": role, "content": blocks})
	}
	for _, item := range list {
		m, ok := item.(object)
		if !ok {
			return nil, nil, errors.New("messages: entry is not an object")
		}
		blocks, err := contentBlocks(m["content"])
		if err != nil {
			return nil, nil, err
		}
		switch m["role"] {
		case "system":
			texts, tools := splitSystemMessage(blocks)
			hoisted = append(hoisted, tools...)
			if n := len(out); n > 0 && out[n-1]["role"] == "user" {
				push("user", texts)
			} else {
				pending = append(pending, texts...)
			}
		case "user":
			kept := filterBlocks(blocks, allowed)
			kept = append(kept, pending...)
			pending = nil
			if len(kept) > 0 {
				push("user", kept)
			}
		case "assistant":
			if kept := filterBlocks(blocks, allowed); len(kept) > 0 {
				push("assistant", kept)
			}
		default:
			return nil, nil, fmt.Errorf("messages: unsupported role %v", m["role"])
		}
	}
	if len(pending) > 0 {
		push("user", pending)
	}
	msgs = make([]any, len(out))
	for i, m := range out {
		msgs[i] = m
	}
	return msgs, hoisted, nil
}

func contentBlocks(c any) ([]any, error) {
	switch v := c.(type) {
	case string:
		return []any{object{"type": "text", "text": v}}, nil
	case []any:
		return v, nil
	default:
		return nil, errors.New("messages: content must be a string or an array")
	}
}

func splitSystemMessage(blocks []any) (texts, tools []any) {
	for _, b := range blocks {
		blk, ok := b.(object)
		if !ok {
			continue
		}
		switch blk["type"] {
		case "text":
			if text, ok := blk["text"].(string); ok {
				texts = append(texts, object{"type": "text", "text": text})
			}
		case "tool_addition":
			tool, _ := blk["tool"].(object)
			if def, ok := tool["definition"].(object); ok && tool["type"] == "tool_definition" {
				tools = append(tools, def)
			}
		}
	}
	return texts, tools
}

// filterBlocks keeps allowed block types, strips cache_control, and filters
// the nested content of tool_result blocks the same way.
func filterBlocks(blocks []any, allowed []string) []any {
	var out []any
	for _, b := range blocks {
		blk, ok := b.(object)
		if !ok {
			continue
		}
		typ, _ := blk["type"].(string)
		if !slices.Contains(allowed, typ) {
			continue
		}
		delete(blk, "cache_control")
		if nested, ok := blk["content"].([]any); ok && typ == "tool_result" {
			if kept := filterBlocks(nested, allowed); len(kept) > 0 {
				blk["content"] = kept
			} else {
				delete(blk, "content") // optional; null would be rejected
			}
		}
		out = append(out, blk)
	}
	return out
}

// cleanTools keeps client tools only, reduced to {name, description,
// input_schema}. Server tools (any non-custom type) and deferred tools are
// dropped. Later definitions with the same name replace earlier ones.
func cleanTools(v any, hoisted []any) ([]any, error) {
	var list []any
	if v != nil {
		l, ok := v.([]any)
		if !ok {
			return nil, errors.New("tools: must be an array")
		}
		list = l
	}
	var out []any
	index := map[string]int{}
	for _, t := range append(slices.Clone(list), hoisted...) {
		tool, ok := t.(object)
		if !ok {
			return nil, errors.New("tools: entry is not an object")
		}
		if typ, has := tool["type"]; has && typ != "custom" {
			continue
		}
		if tool["defer_loading"] == true {
			continue
		}
		name, _ := tool["name"].(string)
		if name == "" {
			return nil, errors.New("tools: tool without a name")
		}
		clean := object{"name": name}
		for _, k := range []string{"description", "input_schema"} {
			if val, ok := tool[k]; ok {
				clean[k] = val
			}
		}
		stripSchemaRegex(clean["input_schema"])
		if i, seen := index[name]; seen {
			out[i] = clean
			continue
		}
		index[name] = len(out)
		out = append(out, clean)
	}
	return out, nil
}

// stripSchemaRegex removes JSON Schema regex keywords (pattern,
// patternProperties) from every schema node. Providers validate them with
// their own regex dialect and reject the whole request on constructs such as
// "\0" (seen with Claude Code's Artifact tool). Keywords are only removed at
// schema positions, so a property merely named "pattern" survives.
func stripSchemaRegex(v any) {
	node, ok := v.(object)
	if !ok {
		return
	}
	delete(node, "pattern")
	delete(node, "patternProperties")
	for _, k := range []string{"properties", "$defs", "definitions", "dependentSchemas", "dependencies"} {
		if m, ok := node[k].(object); ok {
			for _, sub := range m {
				stripSchemaRegex(sub)
			}
		}
	}
	for _, k := range []string{
		"items", "additionalItems", "additionalProperties", "unevaluatedItems", "unevaluatedProperties",
		"contains", "propertyNames", "not", "if", "then", "else", "contentSchema",
	} {
		stripSchemaRegex(node[k])
	}
	for _, k := range []string{"anyOf", "allOf", "oneOf", "prefixItems", "items"} {
		if list, ok := node[k].([]any); ok {
			for _, sub := range list {
				stripSchemaRegex(sub)
			}
		}
	}
}

// fixToolChoice drops tool_choice when no tools remain or when it names a
// tool that was removed.
func fixToolChoice(o object, tools []any) {
	tc, ok := o["tool_choice"].(object)
	if !ok {
		delete(o, "tool_choice")
		return
	}
	if len(tools) == 0 {
		delete(o, "tool_choice")
		return
	}
	if tc["type"] != "tool" {
		return
	}
	for _, t := range tools {
		if t.(object)["name"] == tc["name"] {
			return
		}
	}
	delete(o, "tool_choice")
}

// fixThinking maps adaptive/enabled to {"type":"enabled"}; a forced
// tool_choice (any/tool) requires thinking off on DeepSeek.
func fixThinking(o object) {
	if tc, ok := o["tool_choice"].(object); ok && (tc["type"] == "any" || tc["type"] == "tool") {
		o["thinking"] = object{"type": "disabled"}
		return
	}
	th, ok := o["thinking"].(object)
	if !ok {
		delete(o, "thinking")
		return
	}
	switch th["type"] {
	case "adaptive", "enabled":
		o["thinking"] = object{"type": "enabled"}
	case "disabled":
		o["thinking"] = object{"type": "disabled"}
	default:
		delete(o, "thinking")
	}
}

// fixOutputConfig keeps only output_config.effort.
func fixOutputConfig(o object) {
	oc, ok := o["output_config"].(object)
	if effort, has := oc["effort"]; ok && has {
		o["output_config"] = object{"effort": effort}
		return
	}
	delete(o, "output_config")
}
