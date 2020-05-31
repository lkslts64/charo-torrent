package bencode

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
)

const benStringStart string = "0123456789"

var benElems = map[string]string{
	"l": "list ",
	"d": "dictionary",
	"i": "integer",
	"s": "string",
}

//TODO: I should have better error handling.Specifically,
//more error types and particularly an offset variable to
//know where the error occured.

//IncompatibleTypesError is generated when there is a type
//incompatibility between the data structures provided
//and the bencoded data.
type IncompatibleTypesError struct {
	bencType string
	dataType string
}

func (d IncompatibleTypesError) Error() string {
	return fmt.Sprintf("bencType has %s type while dataType is %s", d.bencType, d.dataType)
}

type LargeBufferErr struct {
	RemainingLen int
}

func (e *LargeBufferErr) Error() string {
	return fmt.Sprintf("Remaining length: %d\n", e.RemainingLen)
}

//UnknownValueError  is generated when a bencoded
//value starts with a byte not included in {l,d,i,0,1,2,3,4,5,6,7,8,9}
type UnknownValueError struct {
	symbol string
}

func (u UnknownValueError) Error() string {
	return fmt.Sprintf("unknown bencoded element starting with %s", u.symbol)
}

func Decode(data []byte, v interface{}) error {
	e := reflect.ValueOf(v)
	if e.Type().Kind() != reflect.Ptr {
		return errors.New("bencode: v should have a pointer type")
	}
	val := reflect.ValueOf(v)
	if !val.IsValid() {
		return errors.New("bencode: provided pointers is nil")
	}
	r := benReader{bytes.NewBuffer(data)}
	err := decode(r, val.Elem())
	if err != nil {
		return fmt.Errorf("bencode: %w", err)
	}
	_, err = r.b.ReadByte()
	if err == nil {
		return &LargeBufferErr{
			RemainingLen: r.b.Len() + 1, //+1 cause we read one byte
		}
	}
	if err != io.EOF {
		return fmt.Errorf("bencode: read last byte error other than EOF: %w", err)
	}
	return nil
}

//Parse the bencoded string based on v.
//That means that we will expect each bencoded value
//to have type compatible with v (and not the opposite).
func decode(r benReader, v reflect.Value) error {
	if !v.IsValid() {
		panic("did not expected zero value at start of decode func.Developer's mistake!")
	}
	if !v.CanSet() {
		panic("did not expexpected non settable value at start of decode func.Developer's mistake!")
	}
	t := v.Type()
	switch v.Kind() {
	//TODO: handle properly interface types ( nil - empty interfaces)
	case reflect.Interface:
		if v.NumMethod() == 0 {
			err := handleNilInterface(r, v)
			if err != nil {
				return err
			}
		} else {
			return errors.New("cant handle non empty interfaces right now")
		}
	case reflect.Ptr:
		//if pointer is nil,create a new zeroed (but not nil) value `e` of type `v`
		//and pass `e.Elem()` to decode. After, set `e` to `v`.
		if !v.Elem().IsValid() {
			e := reflect.New(t.Elem())
			if err := decode(r, e.Elem()); err != nil {
				return err
			}
			v.Set(e)
		} else if err := decode(r, v.Elem()); err != nil {
			return err
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		num, err := r.readBenInt()
		if err != nil {
			return err
		}
		v.SetInt(num)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		unum, err := r.readBenUint()
		if err != nil {
			return err
		}
		v.SetUint(unum)
	case reflect.Bool:
		bnum, err := r.readBenBool()
		if err != nil {
			return err
		}
		v.SetBool(bnum)
	case reflect.String:
		bytes, err := r.readBenString()
		if err != nil {
			return err
		}
		v.SetString(string(bytes))
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			bytes, err := r.readBenString()
			if err != nil {
				return err
			}
			v.SetBytes(bytes)
			break
		}
		err := r.readBenList(v)
		if err != nil {
			return err
		}
	case reflect.Map:
		err := r.readBenDictMap(v)
		if err != nil {
			return err
		}
	case reflect.Struct:
		err := r.readBenDictStruct(v)
		if err != nil {
			return err
		}
	}
	return nil
}

type benReader struct {
	b *bytes.Buffer
}

func (r benReader) readBenString() ([]byte, error) {
	err := r.assertBenElem('s')
	if err != nil {
		return nil, err
	}
	err = r.b.UnreadByte()
	if err != nil {
		return nil, err
	}
	lenbytes, err := r.b.ReadString(byte(':'))
	if err != nil {
		return nil, err
	}
	strLen, err := strconv.ParseInt(lenbytes[:len(lenbytes)-1], 10, 64)
	if err != nil {
		return nil, err
	}
	str := r.b.Next(int(strLen))
	if len(str) != int(strLen) {
		return nil, errors.New("length of bencoded string does not correspond to his actual length")
	}
	return str, nil
}

func (r benReader) readBenInt() (int64, error) {
	err := r.assertBenElem('i')
	if err != nil {
		return -1, err
	}
	benInt, err := r.b.ReadString(byte('e'))
	if err != nil {
		return -1, err
	}
	num, err := strconv.ParseInt(benInt[:len(benInt)-1], 10, 64)
	if err != nil {
		return -1, err
	}
	return num, nil

}

func (r benReader) readBenUint() (uint64, error) {
	err := r.assertBenElem('i')
	if err != nil {
		return 0, err
	}
	benInt, err := r.b.ReadString(byte('e'))
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(benInt[:len(benInt)-1], 10, 64)
}

func (r benReader) readBenBool() (bool, error) {
	err := r.assertBenElem('i')
	if err != nil {
		return false, err
	}
	benInt, err := r.b.ReadString(byte('e'))
	if err != nil {
		return false, err
	}
	if len(benInt) != 1 {
		return false, errors.New("bool value with length > 1")
	}
	return strconv.ParseBool(string(benInt[0]))
}

//v can be only of type reflect.Slice.
func (r benReader) readBenList(v reflect.Value) error {
	err := r.assertBenElem('l')
	if err != nil {
		return err
	}
	//create a new value whose type is the type of the elements of the slice(v).
	//this is the type that we will expect to be contained in the bencoded string.
	//loop and call decode for each bencoded element that you traverse
	//until you traverse an 'e' who is the first byte of a bencoded value.
	//after decoding, append the decoded value to the slice (v).
	for {
		e := reflect.New(v.Type().Elem()).Elem()
		ok, err := r.checkEnd()
		if err != nil {
			return err
		}
		if ok {
			break
		}
		err = decode(r, e)
		if err != nil {
			return err
		}
		v.Set(reflect.Append(v, e))
	}
	return nil
}

//v can be only of type reflect.Map.
func (r benReader) readBenDictMap(v reflect.Value) error {
	err := r.assertBenElem('d')
	if err != nil {
		return err
	}
	t := v.Type()
	keyType := t.Key()
	elemType := t.Elem()
	if keyType.Kind() != reflect.String {
		return errors.New("maps should have keys of type string")
	}
	//create two zeroed values - the first represents the
	//map's key type and the second the map's value's type.
	//These are the types that we expect the bencoded string to have.
	keyVal := reflect.New(keyType).Elem()
	elemVal := reflect.New(elemType).Elem()
	//Iterate and start decoding values until you see an 'e' as the
	//first byte of a bencoded value.
	for {
		ok, err := r.checkEnd()
		if err != nil {
			return err
		}
		if ok {
			break
		}
		err = decode(r, keyVal)
		if err != nil {
			return err
		}
		err = decode(r, elemVal)
		if err != nil {
			return err
		}
		if v.IsNil() {
			m := reflect.MapOf(keyType, elemType)
			_v := reflect.MakeMap(m)
			v.Set(_v)
		}
		v.SetMapIndex(keyVal, elemVal)
	}
	return nil
}

//v can be only of type reflect.Struct.
func (r benReader) readBenDictStruct(v reflect.Value) error {
	err := r.assertBenElem('d')
	if err != nil {
		return err
	}
	nonOmit, fnames := structInfo(v)
	//decode loop
	var benKey []byte
	for {
		ok, err := r.checkEnd()
		if err != nil {
			return err
		}
		if ok {
			break
		}
		benKey, err = r.readBenString()
		if err != nil {
			return err
		}
		sbenKey := string(benKey)
		//if there is a struct field with the same name as bencoded key,
		//decode the field value .If there are multiple struct keys
		//corresponding to the benKey, then find whats the appropriate one
		// and decode it. If the key is mandatory then delete the key from the
		//nonOmit dict.
		if fname, ok := fnames[sbenKey]; ok {
			var flag bool
			names := strings.Split(fname, "?")
			switch len(names) {
			default:
				for _, fname := range names {
					//error if at least one of them hasn't empty:'omit'
					if _, ok = nonOmit[fname]; ok {
						return errors.New("multiple fields with the same benTag and at least one of them is mandatory")
					}
					fv := v.FieldByName(fname)
					if !fv.IsValid() {
						panic("Developer mistake.Decode struct: field with name " + fname + " doesnt exist in struct")
					}
					lenBefore := r.b.Len()
					err = decode(r, fv)
					if err == nil {
						flag = true
						break
					}
					for j := r.b.Len(); j < lenBefore; j++ {
						err = r.b.UnreadByte()
						if err != nil {
							return err
						}
					}
				}
				if flag == false {
					return errors.New("struct: duplicate tag names: all incompatible with bendata")
				}
			case 1:
				_, ok = nonOmit[fname]
				if ok {
					delete(nonOmit, fname)
				}
				fv := v.FieldByName(fname)
				if !fv.IsValid() {
					panic("Developer mistake.Decode struct: Got field value zero with fname: " + fname)
				}
				err = decode(r, fv)
				if err != nil {
					return fmt.Errorf("struct field %s: %w", fv.Type().Name(), err)
				}
			}
		} else {
			//TODO:maybe just continue and not error - we should read and discard benElem.
			var i interface{}
			v := reflect.New(reflect.ValueOf(&i).Type().Elem()).Elem()
			err := handleNilInterface(r, v)
			if err != nil {
				return err
			}
			continue
		}
	}
	//check if all mandatory fields were present in bencoded buffer.
	if len(nonOmit) != 0 {
		var s string
		for k := range nonOmit {
			s = s + "," + k
		}
		return errors.New("some fields of the struct were not filled and they were not to be ommited: " + s)
	}
	return nil
}

func (r benReader) checkEnd() (bool, error) {
	var b byte
	var err error
	if b, err = r.b.ReadByte(); err != nil {
		return false, err
	}
	if b == 'e' {
		return true, nil
	}
	err = r.b.UnreadByte()
	if err != nil {
		return false, err
	}
	return false, nil
}

//what is always one of {l,d,i,0123456789}.
//b can be any of {l,d,i,0,1,2,3,4,5,6,7,8,9} in order to be valid.
func (r benReader) assertBenElem(what byte) error {
	b, err := r.b.ReadByte()
	if err != nil {
		return err
	}
	switch what {
	case 'i', 'd', 'l':
		if b != what {
			s, err := benElemBasedOnFirstByte(b)
			if err != nil {
				return err
			}
			return &IncompatibleTypesError{s, benElems[string(what)]}
		}
	case 's':
		if !strings.Contains(benStringStart, string(b)) {
			s, err := benElemBasedOnFirstByte(b)
			if err != nil {
				return err
			}
			return &IncompatibleTypesError{s, benElems[string(what)]}
		}
	//debug
	default:
		panic("assertBenElem: argument wasn't any of {l,i,d,s}. Developers mistake")
	}
	return nil
}

func structInfo(v reflect.Value) (map[string]struct{}, map[string]string) {
	t := v.Type()
	nonOmit := map[string]struct{}{}
	fnames := map[string]string{}
	//Store which struct fields must always be present in the bencoded buffer as keys (nonOmit)
	//and what names the struct fields should have (fnames).
	for i := 0; i < v.NumField(); i++ {
		sf := t.Field(i)
		key := sf.Tag.Get("bencode")
		if key == "-" {
			continue
		}
		if ekey := sf.Tag.Get("empty"); ekey != "omit" {
			nonOmit[sf.Name] = struct{}{}
		}
		//if key already exists, concatenate it
		//with the previous ones with delimeter '?'
		if key == "" {
			key = sf.Name
		}
		v, ok := fnames[key]
		if ok {
			fnames[key] = v + "?" + sf.Name
		} else {
			fnames[key] = sf.Name
		}
	}
	return nonOmit, fnames
}

func benElemBasedOnFirstByte(b byte) (string, error) {
	switch {
	case b == 'i', b == 'l', b == 'd':
		return benElems[string(b)], nil
	case b >= '0' && b <= '9':
		return "string", nil
	default:
		return "", &UnknownValueError{string(b)}
	}
}

//if we have a nil interface, then we dont
//know which bencoded type to expect. What
//we can do is set the interface to the type
//the next bencoded value will be.
func handleNilInterface(r benReader, v reflect.Value) error {
	b, err := r.b.ReadByte()
	if err != nil {
		return err
	}
	switch {
	case b == 'i':
		var num int64
		err = setNilInterface(r, v, reflect.ValueOf(&num).Elem())
	case b >= '0' && b <= '9':
		var s string
		err = setNilInterface(r, v, reflect.ValueOf(&s).Elem())
	case b == 'l':
		err = setNilInterface(r, v, reflect.ValueOf(&[]interface{}{}).Elem())
	case b == 'd':
		err = setNilInterface(r, v, reflect.ValueOf(&map[string]interface{}{}).Elem())
	default:
		return &UnknownValueError{string(b)}
	}
	if err != nil {
		return fmt.Errorf("eface decode: %w", err)
	}
	return nil
}

//We know what type the interface should have (see handleNilInterface),
//so we call decode to get the data from the bencoded buffer and after
//we Set the data to the interface (i).
func setNilInterface(r benReader, i, v reflect.Value) error {
	err := r.b.UnreadByte()
	if err != nil {
		return err
	}
	err = decode(r, v)
	if err != nil {
		return err
	}
	i.Set(v)
	return nil
}
