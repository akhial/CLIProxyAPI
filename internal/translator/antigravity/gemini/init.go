package gemini

import (
	"errors"

	"github.com/MoeForge/moego/ojson"
	. "github.com/router-for-me/CLIProxyAPI/v8/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/translator/translator"
)

func init() {
	translator.Register(
		Gemini,
		Antigravity,
		func(_ string, body []byte, _ bool) ([]byte, error) {
			doc, err := NewRequestDoc(body)
			if err != nil {
				return nil, err
			}
			return doc.Bytes(), nil
		},
		interfaces.TranslateResponse{
			Stream:     ConvertAntigravityResponseToGemini,
			NonStream:  ConvertAntigravityResponseToGeminiNonStream,
			TokenCount: GeminiTokenCount,
		},
	)
}

// NewRequestDoc wraps an untouched native Gemini request for Cloud Code.
func NewRequestDoc(payload []byte) (*ojson.Doc, error) {
	if !ojson.ValidBytes(payload) || !ojson.ParseBytes(payload).IsObject() {
		return nil, errors.New("expected a Gemini request object")
	}
	doc := ojson.NewDocString("{}")
	if err := doc.SetRawBytes("request", payload); err != nil {
		return nil, err
	}
	return doc, nil
}
