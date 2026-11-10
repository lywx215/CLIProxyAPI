package gemini

// ContentCount inspects the array for the single-turn mode check. Media
// routing uses the incremental scanner, independently of history size.
func ContentCount(data []byte) int {
	root, err := rawObject(data)
	if err != nil {
		return 0
	}
	contents, err := arrayCursor(root["contents"])
	if err != nil {
		return 0
	}
	count := 0
	for contents.more() {
		contents.next()
		count++
	}
	return count
}
