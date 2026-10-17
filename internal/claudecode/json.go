package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// object is a JSON object that keeps its key order, so rewriting a settings
// file changes only what this program touches. Values are *object, []any,
// string, json.Number, bool or nil.
type object struct {
	keys []string
	vals map[string]any
}

func newObject() *object { return &object{vals: map[string]any{}} }

// obj builds an object from key, value pairs in order.
func obj(kv ...any) *object {
	o := newObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.set(kv[i].(string), kv[i+1])
	}
	return o
}

func (o *object) get(k string) (any, bool) {
	v, ok := o.vals[k]
	return v, ok
}

// set replaces the value of k in place, or appends k.
func (o *object) set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *object) del(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, x := range o.keys {
		if x == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			return
		}
	}
}

// child returns the object under k, creating it when missing. A value of
// another type is an error: the file is not what Claude Code writes.
func (o *object) child(k string) (*object, error) {
	v, ok := o.get(k)
	if !ok {
		c := newObject()
		o.set(k, c)
		return c, nil
	}
	c, ok := v.(*object)
	if !ok {
		return nil, fmt.Errorf("%q is not an object", k)
	}
	return c, nil
}

// list returns the array under k, empty when missing.
func (o *object) list(k string) ([]any, error) {
	v, ok := o.get(k)
	if !ok {
		return nil, nil
	}
	l, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%q is not an array", k)
	}
	return l, nil
}

// parseObject reads a JSON document whose top level is an object. A UTF-8
// byte order mark and an empty document are accepted.
func parseObject(b []byte) (*object, error) {
	b = bytes.TrimPrefix(b, []byte("\xEF\xBB\xBF"))
	if len(bytes.TrimSpace(b)) == 0 {
		return newObject(), nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	v, err := readValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("data after the top-level object")
	}
	o, ok := v.(*object)
	if !ok {
		return nil, errors.New("top level is not an object")
	}
	return o, nil
}

func readValue(dec *json.Decoder) (any, error) {
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t {
	case json.Delim('{'):
		o := newObject()
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			v, err := readValue(dec)
			if err != nil {
				return nil, err
			}
			o.set(kt.(string), v)
		}
		_, err := dec.Token()
		return o, err
	case json.Delim('['):
		l := []any{}
		for dec.More() {
			v, err := readValue(dec)
			if err != nil {
				return nil, err
			}
			l = append(l, v)
		}
		_, err := dec.Token()
		return l, err
	}
	return t, nil
}

// encode writes v indented by two spaces, as Claude Code writes its
// settings, with a final newline.
func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := write(&buf, v, ""); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

func write(buf *bytes.Buffer, v any, indent string) error {
	inner := indent + "  "
	switch x := v.(type) {
	case *object:
		if len(x.keys) == 0 {
			buf.WriteString("{}")
			return nil
		}
		buf.WriteString("{\n")
		for i, k := range x.keys {
			buf.WriteString(inner)
			if err := writeScalar(buf, k); err != nil {
				return err
			}
			buf.WriteString(": ")
			if err := write(buf, x.vals[k], inner); err != nil {
				return err
			}
			if i < len(x.keys)-1 {
				buf.WriteByte(',')
			}
			buf.WriteByte('\n')
		}
		buf.WriteString(indent + "}")
	case []any:
		if len(x) == 0 {
			buf.WriteString("[]")
			return nil
		}
		buf.WriteString("[\n")
		for i, e := range x {
			buf.WriteString(inner)
			if err := write(buf, e, inner); err != nil {
				return err
			}
			if i < len(x)-1 {
				buf.WriteByte(',')
			}
			buf.WriteByte('\n')
		}
		buf.WriteString(indent + "]")
	case json.Number:
		buf.WriteString(x.String())
	default:
		return writeScalar(buf, x)
	}
	return nil
}

// writeScalar writes a string, bool, number or null without HTML escaping.
func writeScalar(buf *bytes.Buffer, v any) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	buf.Write(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
	return nil
}
