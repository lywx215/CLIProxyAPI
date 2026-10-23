package geminienterprise

import "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"

// Responses use the error-returning Parser/Output API. The legacy registry response
// callbacks cannot represent terminal stream failures and must not mask them.
func init() {
	for _, source := range []translator.Format{translator.FormatOpenAI, translator.FormatGemini} {
		translator.Register(source, translator.FormatGeminiEnterprise, func(model string, body []byte, _ bool) ([]byte, error) {
			input, err := Normalize(body, source.String(), model)
			return input.Body, err
		}, translator.ResponseTransform{})
	}
}
