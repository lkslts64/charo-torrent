package bencode

import (
	"bytes"
	"errors"
	"io/ioutil"
	"reflect"
	"sort"
	"strconv"
)

//If a struct field is empty and the user wants it to be ommited from
//the bencoded result, then a struct field tag should be added like this:
//`empty:"omit".If a struct field should be ommited regardless of the its
//emptiness, then a tag should be added like: `bencode:"-"
func Encode(v interface{}) ([]byte, error) {
	var b bytes.Buffer
	val := reflect.ValueOf(v)
	if !val.IsValid() {
		return []byte(""), nil
	}
	err := encode(val, &b)
	if err != nil {
		return nil, err
	}
	return ioutil.ReadAll(&b)

}

//structs encode to bencoded dictionaries.
//In struct fields, if a struct tag with key=='bencode' is
//present, then assume the key is the value of the tag. Otherwise,
//the struct field's name will be the dicionary's key.
//TODO: Support bigInts? (they are needed to parse .torrent files who talk about huge pieces or files)
//int64 should be enough for most of the torrents sizes.
func encode(v reflect.Value, b *bytes.Buffer) error {
	if !v.IsValid() {
		panic("did not expected zero value at start of encode func.Developers mistake!")
	}
	t := v.Type()
	switch t.Kind() {
	//'dereference' pointer.
	case reflect.Ptr:
		if !v.Elem().IsValid() { //propably equivilent to v.isNil()
			handleNilPtr(t, b)
		} else if err := encode(v.Elem(), b); err != nil {
			return err
		}
	//look inside the interface.
	case reflect.Interface:
		//ignore nil interfaces?- tricky decision
		if !v.Elem().IsValid() {
			break
		}
		if err := encode(v.Elem(), b); err != nil {
			return err
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		s := strconv.FormatInt(v.Int(), 10)
		b.WriteString("i" + s + "e")
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		s := strconv.FormatUint(v.Uint(), 10)
		b.WriteString("i" + s + "e")
	case reflect.Bool:
		if v.Bool() {
			b.WriteString("i1e")
			break
		}
		b.WriteString("i0e")
	case reflect.String:
		b.WriteString(strconv.Itoa(len(v.String())) + ":" + v.String())
	case reflect.Slice:
		//if it's a slice of Uint8 (aka bytes), then encode as string.
		//else, as bencode type.
		if t.Elem().Kind() == reflect.Uint8 {
			s := v.Bytes()
			b.WriteString(strconv.Itoa(len(s)) + ":" + string(s))
			break
		}
		if t.Elem().Kind() != reflect.Uint8 {
			if v.IsNil() {
				b.WriteString("le")
				break
			}
			b.WriteString("l")
			for i := 0; i < v.Len(); i++ {
				if err := encode(v.Index(i), b); err != nil {
					return err
				}

			}
		}
		b.WriteString("e")
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return errors.New("map keys are not of type string")
		}
		if v.IsNil() {
			b.WriteString("de")
		}
		keys := string_reflect(v.MapKeys())
		sort.Sort(keys)
		b.WriteString("d")
		for i := 0; i < len(keys); i++ {
			if err := encode(keys[i], b); err != nil {
				return err
			}
			if err := encode(v.MapIndex(keys[i]), b); err != nil {
				return err
			}
		}
		b.WriteString("e")
	//treat struct like dicts - field name is the key of the dict.
	case reflect.Struct:
		if v.NumField() == 0 {
			b.WriteString("de")
		}
		sf := make(sfield_slice, v.NumField())
		for i := 0; i < v.NumField(); i++ {
			sf[i] = sfield(t.Field(i))
		}
		sort.Sort(sf)
		b.WriteString("d")
		for i := 0; i < v.NumField(); i++ {
			fvalue := v.FieldByName(sf[i].Name)
			//if bencode tag == "-" OR field is a nil pointer and struct tag == omitempty , then ignore this field.
			if sf[i].Tag.Get("bencode") == "-" || (sf[i].Tag.Get("empty") == "omit" && isZero(fvalue)) {
				continue
			}
			//encode string and field
			b.WriteString(strconv.Itoa(len(sf.get(i))) + ":" + sf.get(i))
			if err := encode(fvalue, b); err != nil {
				return err
			}
		}
		b.WriteString("e")
	default:
		return errors.New("Unsupported type")
	}
	return nil
}

func handleNilPtr(t reflect.Type, b *bytes.Buffer) {
	e := t.Elem()
	switch e.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		b.WriteString("i0e")
	case reflect.Slice:
		if e.Elem().Kind() != reflect.Uint8 {
			b.WriteString("le")
			break

		}
		fallthrough
	case reflect.String:
		b.WriteString("0:")
	case reflect.Struct:
		b.WriteString("de")

	}
}

//isZero checks if v is a zero valued type
//e.g if i is integer and i has value 0 then
//isZero returns true. This function was copied
//verbatim from stack-overflow.
func isZero(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Func, reflect.Map, reflect.Slice:
		return v.IsNil()
	case reflect.Array:
		z := true
		for i := 0; i < v.Len(); i++ {
			z = z && isZero(v.Index(i))
		}
		return z
	case reflect.Struct:
		z := true
		for i := 0; i < v.NumField(); i++ {
			z = z && isZero(v.Field(i))
		}
		return z
	}
	// Compare other types directly:
	z := reflect.Zero(v.Type())
	return v.Interface() == z.Interface()
}

type string_reflect []reflect.Value

func (s string_reflect) Len() int           { return len(s) }
func (s string_reflect) Less(i, j int) bool { return s[i].String() < s[j].String() }
func (s string_reflect) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

type sfield reflect.StructField

type sfield_slice []sfield

func (s sfield_slice) Len() int           { return len(s) }
func (s sfield_slice) Less(i, j int) bool { return s.get(i) < s.get(j) }
func (s sfield_slice) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

//if a tag with key == 'bencode' is present,
//then return the tag value. Otherwise, return
//the struct field's name.
func (s sfield_slice) get(i int) string {
	var str string
	if str = s[i].Tag.Get("bencode"); str == "" {
		return s[i].Name
	}
	return str
}
