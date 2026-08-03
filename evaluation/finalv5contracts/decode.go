package finalv5contracts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

const maxContractBytes = 32 << 20

func decodeStrictJSONFile(path string, destination any) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("contract is not a regular file")
	}
	if info.Size() > maxContractBytes {
		return nil, fmt.Errorf("contract exceeds %d bytes", maxContractBytes)
	}
	value, err := io.ReadAll(io.LimitReader(file, maxContractBytes+1))
	if err != nil {
		return nil, err
	}
	if len(value) > maxContractBytes {
		return nil, fmt.Errorf("contract exceeds %d bytes", maxContractBytes)
	}
	if err := decodeStrictJSON(value, destination); err != nil {
		return nil, err
	}
	return value, nil
}

func decodeStrictJSON(value []byte, destination any) error {
	if len(bytes.TrimSpace(value)) == 0 || !json.Valid(value) {
		return errors.New("contract is not valid JSON")
	}
	if err := rejectDuplicateJSONKeys(value); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("contract contains a trailing JSON value")
		}
		return fmt.Errorf("contract contains trailing content: %w", err)
	}
	return nil
}

func rejectDuplicateJSONKeys(value []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var consume func() error
	consume = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("JSON object key is not a string")
				}
				if seen[key] {
					return fmt.Errorf("contract contains duplicate JSON key %q", key)
				}
				seen[key] = true
				if err := consume(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := consume(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return errors.New("contract contains an invalid JSON delimiter")
		}
	}
	if err := consume(); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("contract contains trailing JSON")
	}
	return nil
}

func decodeRawObject(value json.RawMessage) (map[string]any, error) {
	var result map[string]any
	if err := decodeStrictJSON(value, &result); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("value is not a JSON object")
	}
	return result, nil
}

func decodeAny(value []byte) (any, error) {
	var result any
	if err := decodeStrictJSON(value, &result); err != nil {
		return nil, err
	}
	return result, nil
}
