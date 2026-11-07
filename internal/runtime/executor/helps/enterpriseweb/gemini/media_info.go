package gemini

// InlineMediaInfo inspects raw JSON views without copying large base64 strings.
func InlineMediaInfo(data []byte) (bool, int) {
	root, err := rawObject(data)
	if err != nil {
		return false, 0
	}
	contents, err := rawArray(root["contents"], maxHistorySegments)
	if err != nil {
		return false, 0
	}
	found := false
	for _, raw := range contents {
		content, err := rawObject(raw)
		if err != nil {
			continue
		}
		parts, err := rawArray(content["parts"], 4096)
		if err != nil {
			continue
		}
		for _, raw := range parts {
			part, err := rawObject(raw)
			if err != nil {
				continue
			}
			_, camel := part["inlineData"]
			_, snake := part["inline_data"]
			_, file := part["fileData"]
			_, fileSnake := part["file_data"]
			if camel || snake || file || fileSnake {
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	return found, len(contents)
}
