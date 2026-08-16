package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	maxPathBytes    = 4096
	maxSegmentBytes = 255
)

// UserPath is a canonical provider-independent absolute virtual path.
// Its zero value is invalid; construct it with ParseUserPath.
type UserPath struct {
	normalized string
}

// ParseUserPath validates and NFC-normalizes a virtual path.
func ParseUserPath(value string) (UserPath, error) {
	if !utf8.ValidString(value) {
		return UserPath{}, NewError(ErrorInvalid, "path must be valid UTF-8")
	}
	value = norm.NFC.String(value)
	if value == "/" {
		return UserPath{normalized: value}, nil
	}
	if value == "" || value[0] != '/' {
		return UserPath{}, NewError(ErrorInvalid, "path must be absolute")
	}
	if len(value) > maxPathBytes {
		return UserPath{}, NewError(ErrorInvalid, "path exceeds 4096 UTF-8 bytes")
	}
	if strings.HasSuffix(value, "/") {
		return UserPath{}, NewError(ErrorInvalid, "path must not have a trailing separator")
	}

	segments := strings.Split(value[1:], "/")
	for index, segment := range segments {
		if err := validatePathSegment(segment); err != nil {
			return UserPath{}, fmt.Errorf("segment %d: %w", index+1, err)
		}
	}
	if strings.EqualFold(segments[0], ".endlessfs") || strings.EqualFold(segments[0], ".trash") {
		return UserPath{}, NewError(ErrorInvalid, "reserved top-level path")
	}
	return UserPath{normalized: value}, nil
}

func validatePathSegment(segment string) error {
	if segment == "" {
		return NewError(ErrorInvalid, "empty path segment")
	}
	if len(segment) > maxSegmentBytes {
		return NewError(ErrorInvalid, "path segment exceeds 255 UTF-8 bytes")
	}
	if segment == "." || segment == ".." {
		return NewError(ErrorInvalid, "dot path segment")
	}
	for _, character := range segment {
		if character == '\\' || character == 0 || character < 0x20 || character == 0x7f {
			return NewError(ErrorInvalid, "forbidden path character")
		}
	}
	return nil
}

// MustParseUserPath parses value and panics when it is invalid. It is intended
// for trusted constants and test fixtures, not request input.
func MustParseUserPath(value string) UserPath {
	path, err := ParseUserPath(value)
	if err != nil {
		panic(err)
	}
	return path
}

func (p UserPath) String() string {
	return p.normalized
}

func (p UserPath) IsRoot() bool {
	return p.normalized == "/"
}

func (p UserPath) Valid() bool {
	return p.normalized != ""
}

func (p UserPath) Segments() []string {
	if p.IsRoot() {
		return nil
	}
	if !p.Valid() {
		return nil
	}
	return strings.Split(p.normalized[1:], "/")
}

func (p UserPath) Name() string {
	segments := p.Segments()
	if len(segments) == 0 {
		return ""
	}
	return segments[len(segments)-1]
}

func (p UserPath) Parent() UserPath {
	if !p.Valid() || p.IsRoot() {
		return UserPath{normalized: "/"}
	}
	index := strings.LastIndexByte(p.normalized, '/')
	if index == 0 {
		return UserPath{normalized: "/"}
	}
	return UserPath{normalized: p.normalized[:index]}
}

func (p UserPath) Join(segment string) (UserPath, error) {
	if !p.Valid() {
		return UserPath{}, NewError(ErrorInvalid, "invalid base path")
	}
	if !utf8.ValidString(segment) {
		return UserPath{}, NewError(ErrorInvalid, "path segment must be valid UTF-8")
	}
	segment = norm.NFC.String(segment)
	if err := validatePathSegment(segment); err != nil {
		return UserPath{}, err
	}
	if strings.EqualFold(segment, ".endlessfs") || strings.EqualFold(segment, ".trash") {
		if p.IsRoot() {
			return UserPath{}, NewError(ErrorInvalid, "reserved top-level path")
		}
	}
	value := p.normalized + "/" + segment
	if p.IsRoot() {
		value = "/" + segment
	}
	return ParseUserPath(value)
}

// IsDescendantOf reports whether p is strictly below root.
func (p UserPath) IsDescendantOf(root UserPath) bool {
	if !p.Valid() || !root.Valid() || p == root {
		return false
	}
	if root.IsRoot() {
		return !p.IsRoot()
	}
	return strings.HasPrefix(p.normalized, root.normalized+"/")
}

func (p UserPath) MarshalJSON() ([]byte, error) {
	if !p.Valid() {
		return nil, NewError(ErrorInvalid, "cannot encode invalid path")
	}
	return json.Marshal(p.normalized)
}

func (p *UserPath) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	parsed, err := ParseUserPath(value)
	if err != nil {
		return err
	}
	*p = parsed
	return nil
}

func (p UserPath) GoString() string {
	return fmt.Sprintf("UserPath(%q)", p.normalized)
}
