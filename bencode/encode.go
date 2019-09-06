package bencode

import (
	"bytes"
	"errors"
	"io/ioutil"
	"reflect"
	"sort"
	"strconv"
)

func Encode(v interface{}) ([]byte, error) {
	var b bytes.Buffer
	err := encode(reflect.ValueOf(v), &b)
	if err != nil {
		return nil, err
	}
	return ioutil.ReadAll(&b)

}

//structs encode to bencoded dictionaries.
//In struct fields, if a struct tag with key=='bencode' is
//present, then assume the key is the value of the tag. Otherwise,
//the struct field's name will be the dicionary's key.
func encode(v reflect.Value, b *bytes.Buffer) error {
	t := v.Type()
	switch t.Kind() {
	//i<integer encoded in base ten ASCII>e
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		s := strconv.FormatInt(v.Int(), 10)
		b.WriteString("i" + s + "e")
	//<string length encoded in base ten ASCII>:<string data>
	case reflect.String:
		b.WriteString(strconv.Itoa(len(v.String())) + ":" + v.String())
	// l<bencoded values>e
	case reflect.Slice:
		//if it's a slice of Uint8 (aka bytes), then encode as string.
		//else, as bencode type.
		if t.Elem().Kind() == reflect.Uint8 {
			s := v.Bytes()
			b.WriteString(strconv.Itoa(len(s)) + ":" + string(s))
			return nil
		}
		if v.IsNil() {
			b.WriteString("le")
			return nil
		}
		b.WriteString("l")
		for i := 0; i < v.Len(); i++ {
			if err := encode(v.Index(i), b); err != nil {
				return err
			}

		}
		b.WriteString("e")
	//d<bencoded string><bencoded element>e
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return errors.New("map keys are not of type string")
		}
		if v.IsNil() {
			b.WriteString("de")
		}
		//one way of doing it
		/*
			keys := v.MapKeys()
			skeys := make([]string, len(keys))
			for i := 0; i < len(keys); i++ {
				skeys[i] = keys[i].String()
			}
			sort.Strings(skeys)
			b.WriteString("d")
			for i := 0; i < len(keys); i++ {
				b.WriteString(strconv.Itoa(len(skeys[i])) + ":" + skeys[i])
				if err := encode(v.MapIndex(keys[i]),v); err != nil {
					return err
				}
			}
			b.WriteString("e")
		*/
		//another one
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
	//d<bencoded string><bencoded element>e
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
			//encode string and field
			b.WriteString(strconv.Itoa(len(sf.get(i))) + ":" + sf.get(i))
			if err := encode(v.FieldByName(sf[i].Name), b); err != nil {
				return err
			}
		}
		b.WriteString("e")
	default:
		return errors.New("Unsupported type")
	}
	return nil
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
