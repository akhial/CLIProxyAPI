package helps

import (
	"bytes"
	"math"
	"strconv"
	"strings"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

// NormalizeAntigravityACPTools normalizes Gemini tool declarations for Cloud Code:
//  1. Expands the single function-only tool emitted by stock ACP into individual
//     Cloud Code tools (one functionDeclaration per tool entry).
//  2. Converts JSON Schema parametersJsonSchema declarations into Cloud Code's
//     typed parameters schema format (uppercase types, stringified int64 bounds,
//     unsupported JSON Schema keywords removed).
func NormalizeAntigravityACPTools(doc *ojson.Doc) error {
	if doc == nil {
		return nil
	}
	tools := doc.Get("request.tools")
	if !tools.IsArray() || tools.Get("#").Int() == 0 {
		return nil
	}

	toolItems := tools.Array()
	splitACPGroup := false
	if len(toolItems) == 1 && agyConversationID(doc) != "" {
		tool := toolItems[0]
		declarations := tool.Get("functionDeclarations")
		if declarations.IsArray() && declarations.Get("#").Int() >= 2 && len(tool.Map()) == 1 {
			splitACPGroup = true
		}
	}

	type functionTool struct {
		Declarations [1]json.RawMessage `json:"functionDeclarations"`
	}

	if splitACPGroup {
		functions := toolItems[0].Get("functionDeclarations").Array()
		groups := make([]functionTool, len(functions))
		for i, function := range functions {
			groups[i].Declarations[0] = json.RawMessage(convertACPFunctionDeclaration(function.Raw))
		}
		return doc.Set("request.tools", groups)
	}

	modified := false
	rebuilt := make([]json.RawMessage, len(toolItems))
	for i, tool := range toolItems {
		declarations := tool.Get("functionDeclarations")
		if !declarations.IsArray() || declarations.Get("#").Int() == 0 {
			rebuilt[i] = json.RawMessage(tool.Raw)
			continue
		}
		functions := declarations.Array()
		toolModified := false
		convertedDecls := make([]json.RawMessage, len(functions))
		for j, function := range functions {
			updated := convertACPFunctionDeclaration(function.Raw)
			if updated != function.Raw {
				toolModified = true
			}
			convertedDecls[j] = json.RawMessage(updated)
		}
		if !toolModified {
			rebuilt[i] = json.RawMessage(tool.Raw)
			continue
		}
		toolDoc := ojson.NewDoc([]byte(tool.Raw))
		if errSet := toolDoc.Set("functionDeclarations", convertedDecls); errSet != nil {
			return errSet
		}
		rebuilt[i] = json.RawMessage(toolDoc.Bytes())
		modified = true
	}
	if !modified {
		return nil
	}
	return doc.Set("request.tools", rebuilt)
}

func convertACPFunctionDeclaration(raw string) string {
	decl := ojson.Parse(raw)
	if !decl.IsObject() {
		return raw
	}
	if decl.Get("parameters").Exists() {
		return raw
	}
	schema := decl.Get("parametersJsonSchema")
	if !schema.Exists() || !schema.IsObject() {
		return raw
	}
	converted, ok := convertJSONSchemaToGeminiParameters(schema.Raw)
	if !ok {
		return raw
	}
	declDoc := ojson.NewDoc([]byte(raw))
	if errSet := declDoc.SetRawBytes("parameters", converted); errSet != nil {
		return raw
	}
	_ = declDoc.Delete("parametersJsonSchema")
	return declDoc.String()
}

func convertJSONSchemaToGeminiParameters(raw string) ([]byte, bool) {
	if strings.TrimSpace(raw) == "" {
		return nil, false
	}
	if strings.Contains(raw, `"$ref"`) {
		raw = util.InlineLocalRefs(raw)
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var parsed any
	if err := decoder.Decode(&parsed); err != nil {
		return nil, false
	}
	rootMap, ok := parsed.(map[string]any)
	if !ok {
		return nil, false
	}
	converted := convertSchemaNodeToGeminiParameters(rootMap)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(converted); err != nil {
		return nil, false
	}
	encoded := buf.Bytes()
	if len(encoded) > 0 && encoded[len(encoded)-1] == '\n' {
		encoded = encoded[:len(encoded)-1]
	}
	return encoded, true
}

func convertSchemaNodeToGeminiParameters(node map[string]any) map[string]any {
	if node == nil {
		return map[string]any{}
	}
	merged := mergeAllOfSchemaNode(node)
	out := make(map[string]any)

	var typeAnyOf []map[string]any
	if rawType, ok := merged["type"]; ok {
		switch typed := rawType.(type) {
		case string:
			if upper := geminiSchemaType(typed); upper != "" {
				out["type"] = upper
			}
		case []any:
			var nonNull []string
			hasNull := false
			for _, item := range typed {
				s, okStr := item.(string)
				if !okStr {
					continue
				}
				upper := geminiSchemaType(s)
				if upper == "" {
					continue
				}
				if upper == "NULL" {
					hasNull = true
					continue
				}
				nonNull = append(nonNull, upper)
			}
			if hasNull {
				out["nullable"] = true
			}
			switch len(nonNull) {
			case 0:
				if hasNull {
					out["type"] = "NULL"
				}
			case 1:
				out["type"] = nonNull[0]
			default:
				for _, t := range nonNull {
					typeAnyOf = append(typeAnyOf, map[string]any{"type": t})
				}
			}
		}
	}

	if nullable, ok := merged["nullable"].(bool); ok && nullable {
		out["nullable"] = true
	}

	for _, key := range []string{"description", "title", "format", "pattern"} {
		if value, ok := merged[key].(string); ok && value != "" {
			out[key] = value
		}
	}

	var propSet map[string]struct{}
	if rawProps, ok := merged["properties"].(map[string]any); ok {
		props := make(map[string]any, len(rawProps))
		propSet = make(map[string]struct{}, len(rawProps))
		for propName, rawProp := range rawProps {
			switch propVal := rawProp.(type) {
			case map[string]any:
				props[propName] = convertSchemaNodeToGeminiParameters(propVal)
				propSet[propName] = struct{}{}
			case bool:
				if propVal {
					props[propName] = map[string]any{}
					propSet[propName] = struct{}{}
				}
			}
		}
		if len(props) > 0 {
			out["properties"] = props
			if _, hasType := out["type"]; !hasType && len(typeAnyOf) == 0 {
				out["type"] = "OBJECT"
			}
		}
	}

	if rawReq, ok := merged["required"]; ok {
		reqs := convertRequiredList(rawReq, propSet)
		if len(reqs) > 0 {
			out["required"] = reqs
		}
	}

	if rawItems, ok := merged["items"]; ok {
		switch itemsVal := rawItems.(type) {
		case map[string]any:
			out["items"] = convertSchemaNodeToGeminiParameters(itemsVal)
		case bool:
			if itemsVal {
				out["items"] = map[string]any{}
			}
		case []any:
			for _, elem := range itemsVal {
				if elemMap, okMap := elem.(map[string]any); okMap {
					out["items"] = convertSchemaNodeToGeminiParameters(elemMap)
					break
				}
			}
		}
	}
	if out["type"] == "ARRAY" && out["items"] == nil {
		out["items"] = map[string]any{"type": "STRING"}
	}

	var branches []map[string]any
	branches = append(branches, typeAnyOf...)
	for _, unionKey := range []string{"anyOf", "oneOf"} {
		rawUnion, ok := merged[unionKey].([]any)
		if !ok {
			continue
		}
		for _, elem := range rawUnion {
			elemMap, okMap := elem.(map[string]any)
			if !okMap {
				continue
			}
			convertedBranch := convertSchemaNodeToGeminiParameters(elemMap)
			if len(convertedBranch) == 1 && convertedBranch["type"] == "NULL" {
				out["nullable"] = true
				continue
			}
			branches = append(branches, convertedBranch)
		}
	}
	if len(branches) == 1 && out["type"] == nil && out["properties"] == nil && out["items"] == nil {
		for k, v := range branches[0] {
			if _, exists := out[k]; !exists {
				out[k] = v
			}
		}
	} else if len(branches) > 0 {
		anyOfList := make([]any, len(branches))
		for i, b := range branches {
			anyOfList[i] = b
		}
		out["anyOf"] = anyOfList
	}

	declaredType, _ := out["type"].(string)
	if declaredType == "" || declaredType == "STRING" {
		if rawEnum, ok := merged["enum"].([]any); ok {
			enumValues := make([]string, 0, len(rawEnum))
			for _, elem := range rawEnum {
				if s, okStr := elem.(string); okStr {
					enumValues = append(enumValues, s)
				}
			}
			if len(enumValues) > 0 {
				out["enum"] = enumValues
				if declaredType == "" {
					out["type"] = "STRING"
				}
			}
		} else if constVal, ok := merged["const"].(string); ok {
			out["enum"] = []string{constVal}
			if declaredType == "" {
				out["type"] = "STRING"
			}
		}
	}

	for _, key := range []string{"minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties"} {
		if rawVal, ok := merged[key]; ok {
			if formatted, okInt := formatProtoInt64Bound(rawVal); okInt {
				out[key] = formatted
			}
		}
	}

	for _, key := range []string{"minimum", "maximum"} {
		if rawVal, ok := merged[key]; ok {
			if numVal, okNum := parseSchemaFloat(rawVal); okNum {
				out[key] = numVal
			}
		}
	}

	if rawOrdering, ok := merged["propertyOrdering"]; ok {
		ordering := convertRequiredList(rawOrdering, nil)
		if len(ordering) > 0 {
			out["propertyOrdering"] = ordering
		}
	}

	if defVal, ok := merged["default"]; ok {
		out["default"] = defVal
	}
	if exVal, ok := merged["example"]; ok {
		out["example"] = exVal
	}

	return out
}

func mergeAllOfSchemaNode(node map[string]any) map[string]any {
	rawAllOf, ok := node["allOf"].([]any)
	if !ok || len(rawAllOf) == 0 {
		return node
	}
	merged := make(map[string]any, len(node))
	for k, v := range node {
		if k != "allOf" {
			merged[k] = v
		}
	}
	var props map[string]any
	if existingProps, okProps := merged["properties"].(map[string]any); okProps {
		props = make(map[string]any, len(existingProps))
		for k, v := range existingProps {
			props[k] = v
		}
	}
	reqs := convertRequiredList(merged["required"], nil)
	seenReq := make(map[string]struct{}, len(reqs))
	for _, r := range reqs {
		seenReq[r] = struct{}{}
	}
	for _, elem := range rawAllOf {
		sub, okSub := elem.(map[string]any)
		if !okSub {
			continue
		}
		sub = mergeAllOfSchemaNode(sub)
		if subProps, okProps := sub["properties"].(map[string]any); okProps {
			if props == nil {
				props = make(map[string]any, len(subProps))
			}
			for pk, pv := range subProps {
				if _, exists := props[pk]; !exists {
					props[pk] = pv
				}
			}
		}
		for _, r := range convertRequiredList(sub["required"], nil) {
			if _, exists := seenReq[r]; !exists {
				seenReq[r] = struct{}{}
				reqs = append(reqs, r)
			}
		}
		for k, v := range sub {
			if k == "properties" || k == "required" || k == "allOf" {
				continue
			}
			if _, exists := merged[k]; !exists {
				merged[k] = v
			}
		}
	}
	if len(props) > 0 {
		merged["properties"] = props
	}
	if len(reqs) > 0 {
		reqAny := make([]any, len(reqs))
		for i, r := range reqs {
			reqAny[i] = r
		}
		merged["required"] = reqAny
	}
	return merged
}

func geminiSchemaType(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "STRING", "NUMBER", "INTEGER", "BOOLEAN", "ARRAY", "OBJECT", "NULL":
		return strings.ToUpper(strings.TrimSpace(raw))
	default:
		return ""
	}
}

func convertRequiredList(raw any, propSet map[string]struct{}) []string {
	var items []string
	switch typed := raw.(type) {
	case []any:
		for _, item := range typed {
			if s, ok := item.(string); ok {
				items = append(items, s)
			}
		}
	case []string:
		items = append(items, typed...)
	default:
		return nil
	}
	out := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, s := range items {
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		if len(propSet) > 0 {
			if _, ok := propSet[s]; !ok {
				continue
			}
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func formatProtoInt64Bound(raw any) (string, bool) {
	switch v := raw.(type) {
	case json.Number:
		if i, err := v.Int64(); err == nil && i >= 0 {
			return strconv.FormatInt(i, 10), true
		}
		if f, err := v.Float64(); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0 && math.Trunc(f) == f {
			return strconv.FormatInt(int64(f), 10), true
		}
	case float64:
		if !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && math.Trunc(v) == v {
			return strconv.FormatInt(int64(v), 10), true
		}
	case int:
		if v >= 0 {
			return strconv.Itoa(v), true
		}
	case int64:
		if v >= 0 {
			return strconv.FormatInt(v, 10), true
		}
	case string:
		trimmed := strings.TrimSpace(v)
		if i, err := strconv.ParseInt(trimmed, 10, 64); err == nil && i >= 0 {
			return strconv.FormatInt(i, 10), true
		}
	}
	return "", false
}

func parseSchemaFloat(raw any) (any, bool) {
	switch v := raw.(type) {
	case json.Number:
		if f, err := v.Float64(); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return v, true
		}
	case float64:
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			return v, true
		}
	case int:
		return v, true
	case int64:
		return v, true
	}
	return nil, false
}
