package ui

import "unicode"

// layoutBaseRune is a limited fallback for terminals that do not report the
// Kitty base-layout key. Terminals with alternate-key support populate
// Key.BaseCode for every layout after inlineRenderer requests alternate-key
// reporting.
func layoutBaseRune(value rune) rune {
	if value == 0 {
		return 0
	}
	value = unicode.ToLower(value)
	if mapped, exists := cyrillicJCUKENBase[value]; exists {
		return mapped
	}
	return value
}

// cyrillicJCUKENBase encodes the ordinary Russian ЙЦУКЕН layout. Other
// Cyrillic layouts (for example Bulgarian BDS or phonetic layouts) may map the
// same rune differently and should not be guessed here.
var cyrillicJCUKENBase = map[rune]rune{
	'ё': '`', 'й': 'q', 'ц': 'w', 'у': 'e', 'к': 'r', 'е': 't', 'н': 'y',
	'г': 'u', 'ш': 'i', 'щ': 'o', 'з': 'p', 'х': '[', 'ъ': ']', 'ф': 'a',
	'ы': 's', 'в': 'd', 'а': 'f', 'п': 'g', 'р': 'h', 'о': 'j', 'л': 'k',
	'д': 'l', 'ж': ';', 'э': '\'', 'я': 'z', 'ч': 'x', 'с': 'c', 'м': 'v',
	'и': 'b', 'т': 'n', 'ь': 'm', 'б': ',', 'ю': '.',
}
