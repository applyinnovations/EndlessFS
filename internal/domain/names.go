package domain

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

type DisplayName struct {
	value string
}

type CredentialLabel struct {
	value string
}

func ParseDisplayName(value string) (DisplayName, error) {
	normalized, err := normalizeHumanLabel(value, 100)
	if err != nil {
		return DisplayName{}, err
	}
	return DisplayName{value: normalized}, nil
}

func ParseCredentialLabel(value string) (CredentialLabel, error) {
	normalized, err := normalizeHumanLabel(value, 64)
	if err != nil {
		return CredentialLabel{}, err
	}
	return CredentialLabel{value: normalized}, nil
}

func normalizeHumanLabel(value string, maxCodePoints int) (string, error) {
	if !utf8.ValidString(value) {
		return "", NewError(ErrorInvalid, "value must be valid UTF-8")
	}
	value = strings.TrimSpace(norm.NFC.String(value))
	if value == "" {
		return "", NewError(ErrorInvalid, "value must not be empty")
	}
	if len(value) > 256 {
		return "", NewError(ErrorInvalid, "value exceeds 256 UTF-8 bytes")
	}
	if utf8.RuneCountInString(value) > maxCodePoints {
		return "", NewError(ErrorInvalid, "value exceeds code-point limit")
	}
	for _, character := range value {
		if unicode.IsControl(character) || character == '\u2028' || character == '\u2029' {
			return "", NewError(ErrorInvalid, "value contains a forbidden character")
		}
	}
	return value, nil
}

func (n DisplayName) String() string {
	return n.value
}

func (l CredentialLabel) String() string {
	return l.value
}

func (n DisplayName) MarshalJSON() ([]byte, error) {
	if n.value == "" {
		return nil, NewError(ErrorInvalid, "cannot encode invalid display name")
	}
	return json.Marshal(n.value)
}

func (n *DisplayName) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	parsed, err := ParseDisplayName(value)
	if err != nil {
		return err
	}
	*n = parsed
	return nil
}

func (l CredentialLabel) MarshalJSON() ([]byte, error) {
	if l.value == "" {
		return nil, NewError(ErrorInvalid, "cannot encode invalid credential label")
	}
	return json.Marshal(l.value)
}

func (l *CredentialLabel) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	parsed, err := ParseCredentialLabel(value)
	if err != nil {
		return err
	}
	*l = parsed
	return nil
}
