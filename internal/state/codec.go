package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

const MaxRecordBytes = 64 << 10

type Validatable interface {
	Validate() error
}

func EncodeJSON(value any) ([]byte, error) {
	if validatable, ok := value.(Validatable); ok {
		if err := validatable.Validate(); err != nil {
			return nil, err
		}
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, domain.WrapError(domain.ErrorInvalid, "encode state record", err)
	}
	if len(data) > MaxRecordBytes {
		return nil, domain.NewError(domain.ErrorInvalid, "state record exceeds size limit")
	}
	return data, nil
}

func DecodeJSON(data []byte, destination any) error {
	return DecodeJSONWithLimit(data, destination, MaxRecordBytes)
}

// DecodeJSONWithLimit applies the same strict duplicate/unknown/trailing-data
// rules to non-state JSON documents with an explicit bounded size.
func DecodeJSONWithLimit(data []byte, destination any, maximumBytes int) error {
	if maximumBytes < 1 || len(data) == 0 || len(data) > maximumBytes {
		return domain.NewError(domain.ErrorInvalid, "invalid state record size")
	}
	if !utf8.Valid(data) {
		return domain.NewError(domain.ErrorInvalid, "state record must be valid UTF-8")
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return domain.WrapError(domain.ErrorInvalid, "invalid state record", err)
	}
	if err := requireEOF(decoder); err != nil {
		return err
	}
	if validatable, ok := destination.(Validatable); ok {
		if err := validatable.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func requireEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return domain.NewError(domain.ErrorInvalid, "trailing state record content")
	}
	return domain.WrapError(domain.ErrorInvalid, "invalid trailing state record content", err)
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return domain.WrapError(domain.ErrorInvalid, "invalid or duplicate state JSON", err)
	}
	return requireEOF(decoder)
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := nameToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, exists := seen[name]; exists {
				return fmt.Errorf("duplicate object key %q", name)
			}
			seen[name] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return errors.New("unexpected JSON delimiter")
	}
}
