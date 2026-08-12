package filesearch

import "unicode/utf8"

func previewLine(text string, limit int) (string, bool) {
	if limit == 0 || len(text) <= limit {
		return text, false
	}
	cut := limit
	for cut > 0 && !utf8.ValidString(text[:cut]) {
		cut--
	}
	return text[:cut], true
}
