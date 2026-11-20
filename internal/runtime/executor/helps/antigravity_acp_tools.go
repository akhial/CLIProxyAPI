package helps

import (
	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
)

// GroupAntigravityACPFunctions expands the single function-only tool emitted by
// stock ACP into individual Cloud Code tools. Copy each declaration intact so
// closed objects, nested schemas, and the client's executable tool set survive.
func GroupAntigravityACPFunctions(doc *ojson.Doc) error {
	tools := doc.Get("request.tools")
	if !tools.IsArray() || tools.Get("#").Int() != 1 {
		return nil
	}
	tool := tools.Get("0")
	declarations := tool.Get("functionDeclarations")
	if !declarations.IsArray() || declarations.Get("#").Int() < 2 || len(tool.Map()) != 1 || agyConversationID(doc) == "" {
		return nil
	}
	type functionTool struct {
		Declarations [1]json.RawMessage `json:"functionDeclarations"`
	}
	functions := declarations.Array()
	groups := make([]functionTool, len(functions))
	for i, function := range functions {
		groups[i].Declarations[0] = json.RawMessage(function.Raw)
	}
	return doc.Set("request.tools", groups)
}
