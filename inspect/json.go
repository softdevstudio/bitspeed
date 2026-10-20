package inspect

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"
)

func decodeStrict(b []byte, target any) error {
	// Token validation runs first because encoding/json otherwise accepts duplicate keys.
	d := json.NewDecoder(bytes.NewReader(b))
	if err := uniqueValue(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("unexpected trailing content")
	}
	if err := requiredFields(b, reflect.TypeOf(target).Elem()); err != nil {
		return err
	}
	return json.Unmarshal(b, target)
}

func uniqueValue(d *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("JSON nesting exceeds limit")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return errors.New("invalid object key")
			}
			if seen[name] {
				return errors.New("duplicate JSON key")
			}
			seen[name] = true
			if err := uniqueValue(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueValue(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	_, err = d.Token()
	return err
}

func requiredFields(raw json.RawMessage, typ reflect.Type) error {
	if typ.Kind() == reflect.Pointer {
		if bytes.Equal(raw, []byte("null")) {
			return nil
		}
		return requiredFields(raw, typ.Elem())
	}
	if bytes.Equal(raw, []byte("null")) {
		return errors.New("null is only allowed for nullable fields")
	}
	if typ == reflect.TypeOf(time.Time{}) {
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return err
		}
		allowed := map[string]bool{}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			tag := strings.Split(f.Tag.Get("json"), ",")
			name := tag[0]
			allowed[name] = true
			value, exists := obj[name]
			optional := len(tag) > 1 && tag[1] == "omitempty"
			if !exists {
				if optional {
					continue
				}
				return fmt.Errorf("missing field %s", name)
			}
			if err := requiredFields(value, f.Type); err != nil {
				return fmt.Errorf("field %s: %w", name, err)
			}
		}
		for key := range obj {
			if !allowed[key] {
				return errors.New("unknown or incorrectly cased field")
			}
		}
	case reflect.Slice:
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return err
		}
		for _, value := range values {
			if err := requiredFields(value, typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}
