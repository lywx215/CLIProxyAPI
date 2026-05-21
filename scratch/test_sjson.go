package main

import (
	"fmt"
	"github.com/tidwall/sjson"
)

func main() {
	out, _ := sjson.Set(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"systemInstruction":{"role":"user","parts":[{"text":"User system prompt"}]}}`, "systemInstruction.parts.-1.text", "Appended system prompt")
	fmt.Println("Append test:", out)
}
