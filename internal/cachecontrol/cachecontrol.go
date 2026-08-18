// Package cachecontrol validates security-relevant HTTP cache policy.
package cachecontrol

import (
	"net/http"
	"strings"
)

// HasNoStore reports whether every Cache-Control field is syntactically valid
// and at least one contains a bare no-store directive. Invalid syntax fails
// closed even when another directive resembles no-store.
func HasNoStore(header http.Header) bool {
	values := header.Values("Cache-Control")
	if len(values) == 0 {
		return false
	}
	found := false
	for _, value := range values {
		fieldNoStore, valid := fieldHasNoStore(value)
		if !valid {
			return false
		}
		found = found || fieldNoStore
	}
	return found
}

func fieldHasNoStore(value string) (bool, bool) {
	found := false
	index := 0
	for {
		index = skipWhitespace(value, index)
		start := index
		for index < len(value) && isTokenCharacter(value[index]) {
			index++
		}
		if start == index {
			return false, false
		}
		name := value[start:index]
		index = skipWhitespace(value, index)
		hasValue := false
		if index < len(value) && value[index] == '=' {
			hasValue = true
			index++
			index = skipWhitespace(value, index)
			var valid bool
			index, valid = skipDirectiveValue(value, index)
			if !valid {
				return false, false
			}
		}
		if strings.EqualFold(name, "no-store") && !hasValue {
			found = true
		}
		index = skipWhitespace(value, index)
		if index == len(value) {
			return found, true
		}
		if value[index] != ',' {
			return false, false
		}
		index++
	}
}

func skipDirectiveValue(value string, index int) (int, bool) {
	if index == len(value) {
		return index, false
	}
	if value[index] != '"' {
		start := index
		for index < len(value) && isTokenCharacter(value[index]) {
			index++
		}
		return index, start != index
	}
	index++
	for index < len(value) {
		character := value[index]
		if character == '"' {
			return index + 1, true
		}
		if character == '\\' {
			index++
			if index == len(value) || !isQuotedCharacter(value[index]) {
				return index, false
			}
		} else if !isQuotedCharacter(character) {
			return index, false
		}
		index++
	}
	return index, false
}

func skipWhitespace(value string, index int) int {
	for index < len(value) && (value[index] == ' ' || value[index] == '\t') {
		index++
	}
	return index
}

func isTokenCharacter(character byte) bool {
	return character >= '0' && character <= '9' ||
		character >= 'A' && character <= 'Z' ||
		character >= 'a' && character <= 'z' ||
		strings.ContainsRune("!#$%&'*+-.^_`|~", rune(character))
}

func isQuotedCharacter(character byte) bool {
	return character == '\t' || character >= ' ' && character != 0x7f
}
