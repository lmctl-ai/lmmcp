package bridge

import (
	"bytes"
	"encoding/json"
	"io"
)

func Decode(data []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	type frame struct {
		object, key bool
		seen        map[string]bool
	}
	stack := []frame{}
	for {
		t, e := d.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return bad("INVALID_JSON")
		}
		if delim, ok := t.(json.Delim); ok {
			switch delim {
			case '{', '[':
				if len(stack) > 0 && stack[len(stack)-1].object {
					stack[len(stack)-1].key = true
				}
				stack = append(stack, frame{object: delim == '{', key: true, seen: map[string]bool{}})
			case '}', ']':
				if len(stack) == 0 {
					return bad("INVALID_JSON")
				}
				stack = stack[:len(stack)-1]
			}
			continue
		}
		if len(stack) > 0 && stack[len(stack)-1].object {
			f := &stack[len(stack)-1]
			if f.key {
				k, ok := t.(string)
				if !ok || f.seen[k] {
					return bad("INVALID_JSON")
				}
				f.seen[k] = true
				f.key = false
			} else {
				f.key = true
			}
		}
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	d.DisallowUnknownFields()
	if d.Decode(dst) != nil {
		return bad("INVALID_JSON")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return bad("INVALID_JSON")
	}
	return nil
}
