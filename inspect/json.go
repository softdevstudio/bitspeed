package inspect

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

func decodeStrict(b []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("unexpected trailing content")
	}
	return nil
}
