package helixqa

import (
	"fmt"
	"regexp"
	"sort"
)

// validateSchema checks doc (a tree of map[string]any / []any / string /
// bool / float64 / nil, as produced by json.Unmarshal into an `any`) against
// schema, returning one human-readable message per violation. An empty
// result means doc is valid.
//
// This is intentionally NOT a general-purpose JSON Schema engine: it
// implements exactly the draft 2020-12 keywords bank_schema.json uses
// ("type", "required", "properties", "additionalProperties", "items",
// "minItems", "minLength", "pattern", "enum") and nothing else. Bringing in
// a third-party schema library would be the more "complete" choice, but
// this suite must stay hermetic and dependency-free (no network access is
// available to `go get` a new module, and RULES requires everything here to
// run without network) -- and the bank shape is small and fixed enough that
// a ~150-line hand-rolled subset is both sufficient and easy to audit.
func validateSchema(schema, doc any) []string {
	return validateNode(schema, doc, "$")
}

func validateNode(schemaAny, doc any, path string) []string {
	schema, ok := schemaAny.(map[string]any)
	if !ok {
		return nil // no object schema at this node: nothing to constrain
	}
	var errs []string

	if t, ok := schema["type"].(string); ok {
		if !typeMatches(t, doc) {
			errs = append(errs, fmt.Sprintf("%s: expected type %q, got %s", path, t, describeType(doc)))
			return errs // further keyword checks are meaningless after a type mismatch
		}
	}

	if enum, ok := schema["enum"].([]any); ok && !enumContains(enum, doc) {
		errs = append(errs, fmt.Sprintf("%s: value not in enum %v", path, enum))
	}

	switch v := doc.(type) {
	case map[string]any:
		errs = append(errs, validateObject(schema, v, path)...)
	case []any:
		errs = append(errs, validateArray(schema, v, path)...)
	case string:
		errs = append(errs, validateString(schema, v, path)...)
	}
	return errs
}

func validateObject(schema map[string]any, obj map[string]any, path string) []string {
	var errs []string

	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			name, _ := r.(string)
			if _, present := obj[name]; !present {
				errs = append(errs, fmt.Sprintf("%s: missing required property %q", path, name))
			}
		}
	}

	props, _ := schema["properties"].(map[string]any)
	if addl, ok := schema["additionalProperties"].(bool); ok && !addl {
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic error ordering
		for _, k := range keys {
			if _, known := props[k]; !known {
				errs = append(errs, fmt.Sprintf("%s: unexpected property %q (additionalProperties: false)", path, k))
			}
		}
	}

	for name, sub := range props {
		if val, present := obj[name]; present {
			errs = append(errs, validateNode(sub, val, path+"."+name)...)
		}
	}
	return errs
}

func validateArray(schema map[string]any, arr []any, path string) []string {
	var errs []string
	if minItems, ok := numeric(schema["minItems"]); ok && float64(len(arr)) < minItems {
		errs = append(errs, fmt.Sprintf("%s: %d item(s), below minItems %v", path, len(arr), minItems))
	}
	if items, ok := schema["items"]; ok {
		for i, el := range arr {
			errs = append(errs, validateNode(items, el, fmt.Sprintf("%s[%d]", path, i))...)
		}
	}
	return errs
}

func validateString(schema map[string]any, s string, path string) []string {
	var errs []string
	if minLen, ok := numeric(schema["minLength"]); ok && float64(len(s)) < minLen {
		errs = append(errs, fmt.Sprintf("%s: length %d below minLength %v", path, len(s), minLen))
	}
	if pat, ok := schema["pattern"].(string); ok {
		re, err := regexp.Compile(pat)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: bank_schema.json has an invalid pattern %q: %v", path, pat, err))
		} else if !re.MatchString(s) {
			errs = append(errs, fmt.Sprintf("%s: %q does not match pattern %q", path, s, pat))
		}
	}
	return errs
}

// numeric normalises a decoded-JSON numeric schema keyword (always a
// float64 once it has passed through json.Unmarshal into `any`) so callers
// don't repeat the type assertion.
func numeric(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func typeMatches(t string, doc any) bool {
	switch t {
	case "object":
		_, ok := doc.(map[string]any)
		return ok
	case "array":
		_, ok := doc.([]any)
		return ok
	case "string":
		_, ok := doc.(string)
		return ok
	case "boolean":
		_, ok := doc.(bool)
		return ok
	case "number":
		_, ok := doc.(float64)
		return ok
	case "integer":
		f, ok := doc.(float64)
		return ok && f == float64(int64(f))
	case "null":
		return doc == nil
	default:
		return true // an unrecognised declared type never blocks validation
	}
}

func describeType(doc any) string {
	switch doc.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%T", doc)
	}
}

func enumContains(enum []any, v any) bool {
	for _, e := range enum {
		if fmt.Sprint(e) == fmt.Sprint(v) {
			return true
		}
	}
	return false
}
