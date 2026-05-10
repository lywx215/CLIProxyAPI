package main

import (
	"fmt"
	"github.com/tidwall/gjson"
)

func main() {
	chunk := []byte(`data: {"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"}}]}`)
	text := gjson.GetBytes(chunk, "candidates.0.content.parts.0.text")
	fmt.Printf("Text: %q (Exists: %v)\n", text.String(), text.Exists())
}
