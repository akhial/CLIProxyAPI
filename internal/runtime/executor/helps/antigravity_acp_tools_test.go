package helps

import (
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
)

func TestNormalizeAntigravityACPToolsSchemaEdgeCases(t *testing.T) {
	doc := ojson.NewDocString(`{
		"request": {
			"tools": [{
				"functionDeclarations": [{
					"name": "complex_tool",
					"description": "Edge cases.",
					"parametersJsonSchema": {
						"$schema": "https://json-schema.org/draft/2020-12/schema",
						"$defs": {
							"Target": {
								"type": "object",
								"properties": {
									"path": {"type": "string", "minLength": 1}
								},
								"required": ["path"]
							}
						},
						"type": "object",
						"allOf": [{
							"properties": {
								"mode": {"const": "fast"},
								"shared": {"type": "string"}
							},
							"required": ["mode"]
						}, {
							"properties": {
								"shared": {"minLength": 2, "description": "Merged constraint."}
							},
							"required": ["shared"]
						}],
						"properties": {
							"target": {"$ref": "#/$defs/Target"},
							"opt": {"anyOf": [{"type": "string"}, {"type": "null"}]},
							"numEnum": {"type": "integer", "enum": [1, 2, 3]},
							"tags": {"type": "array"},
							"nullableStr": {"type": ["string", "null"]}
						}
					}
				}]
			}]
		}
	}`)

	handled, err := NormalizeAntigravityACPTools(doc, true)
	if err != nil {
		t.Fatalf("NormalizeAntigravityACPTools error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}

	params := doc.Get("request.tools.0.functionDeclarations.0.parameters")
	if !params.Exists() {
		t.Fatalf("expected parameters, got: %s", doc.String())
	}
	if doc.Get("request.tools.0.functionDeclarations.0.parametersJsonSchema").Exists() {
		t.Fatal("parametersJsonSchema should be removed")
	}
	if got := params.Get("properties.target.properties.path.type").String(); got != "STRING" {
		t.Fatalf("$ref target.path.type = %q, want STRING", got)
	}
	if got := params.Get("properties.target.properties.path.minLength").String(); got != "1" {
		t.Fatalf("$ref target.path.minLength = %q, want \"1\"", got)
	}
	if got := params.Get("properties.mode.enum.0").String(); got != "fast" {
		t.Fatalf("allOf const mode.enum.0 = %q, want fast", got)
	}
	if got := params.Get("properties.shared.type").String(); got != "STRING" {
		t.Fatalf("allOf merged shared.type = %q, want STRING", got)
	}
	if got := params.Get("properties.shared.minLength").String(); got != "2" {
		t.Fatalf("allOf merged shared.minLength = %q, want \"2\"", got)
	}
	if got := params.Get("properties.shared.description").String(); got != "Merged constraint." {
		t.Fatalf("allOf merged shared.description = %q, want Merged constraint.", got)
	}
	if !params.Get("properties.opt.nullable").Bool() || params.Get("properties.opt.type").String() != "STRING" {
		t.Fatalf("anyOf nullable opt = %s, want STRING + nullable:true", params.Get("properties.opt").Raw)
	}
	if params.Get("properties.numEnum.enum").Exists() {
		t.Fatalf("non-STRING enum should be stripped, got: %s", params.Get("properties.numEnum").Raw)
	}
	if got := params.Get("properties.tags.items.type").String(); got != "STRING" {
		t.Fatalf("missing array items fallback = %q, want STRING", got)
	}
	if !params.Get("properties.nullableStr.nullable").Bool() || params.Get("properties.nullableStr.type").String() != "STRING" {
		t.Fatalf("type array nullable = %s, want STRING + nullable:true", params.Get("properties.nullableStr").Raw)
	}
}

func TestACPSessionCorrelatorExpirationWithMockClock(t *testing.T) {
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	c := newACPSessionCorrelator()
	c.nowFunc = func() time.Time { return now }

	titleSession := c.sessionForTitle("scope-a", "hello")
	now = now.Add(acpPendingPromptTTL + time.Second)

	mainSession := c.sessionForConversation("scope-a", "conv-1", "hello", func() string {
		return "-999"
	})
	if mainSession == titleSession || mainSession != "-999" {
		t.Fatalf("expired title session should not be adopted: title=%q main=%q", titleSession, mainSession)
	}
}
