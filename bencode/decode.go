package bencode

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strconv"
)

//Decode the bencoded string based on v.
//That means that we will expect each bencoded value
//to have type compatible with v (and not the opposite).
func Decode(data []byte, v interface{}) error {
	e := reflect.ValueOf(v)
	if e.Type().Kind() != reflect.Ptr {
		return errors.New("v should have a pointer type")
	}
	val := reflect.ValueOf(v)
	if !val.IsValid() {
		return errors.New("Provided pointers is nil.")
	}
	r := benReader{bytes.NewBuffer(data)}
	err := decode(r, val.Elem())
	if err != nil {
		return err
	}
	_, err = r.b.ReadByte()
	if err == nil || err != io.EOF {
		return errors.New("data structure provided was filled but bencoded buffer wasn't consumed")
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
			return errors.New("Cant handle non empty interfaces right now...")
		}
	case reflect.Ptr:
		//if pointer is nil,create a new zeroed (but not nil) value of type v.Elem()
		//and pass this to decode. After, set this value to v.Elem()
		if !v.Elem().IsValid() {
			e := reflect.New(t.Elem()).Elem()
			if err := decode(r, e); err != nil {
				return err
			}
			v.Elem().Set(e)
			//or v.Set(e.Addr)
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
	lenbytes, err := r.b.ReadString(byte(':'))
	if err != nil {
		return nil, err
	}
	str_len, err := strconv.ParseInt(lenbytes[:len(lenbytes)-1], 10, 64)
	if err != nil {
		return nil, err
	}
	str := r.b.Next(int(str_len))
	if len(str) != int(str_len) {
		return nil, errors.New("length of string does not correspond to his actual length")
	}
	return str, nil
}

func (r benReader) readBenInt() (int64, error) {
	benInt, err := r.b.ReadString(byte('e'))
	if err != nil {
		return -1, err
	}
	if benInt[0] != 'i' {
		return -1, errors.New("Wanted integer but bencoded hasn't. benInt :" + string(benInt))
	}
	num, err := strconv.ParseInt(benInt[1:len(benInt)-1], 10, 64)
	if err != nil {
		return -1, err
	}
	return num, nil

}

func (r benReader) readBenUint() (uint64, error) {
	benInt, err := r.b.ReadString(byte('e'))
	if err != nil {
		return 0, err
	}
	if benInt[0] != 'i' {
		return 0, errors.New("Wanted integer but bencoded hasn't")
	}
	return strconv.ParseUint(benInt[1:len(benInt)-1], 10, 64)
}

func (r benReader) readBenBool() (bool, error) {
	benInt, err := r.b.ReadString(byte('e'))
	if err != nil {
		return false, err
	}
	if len(benInt) != 2 {
		return false, errors.New("Tried to read a Bool but bencoded value wasn't a Bool.")
	}
	if benInt[0] != 'i' {
		return false, errors.New("Wanted integer but bencoded hasn't")
	}
	return strconv.ParseBool(string(benInt[1]))
}

//v can be only of type reflect.Slice.
func (r benReader) readBenList(v reflect.Value) error {
	b, err := r.b.ReadByte()
	if err != nil {
		return err
	}
	if b != 'l' {
		return errors.New("Bencoded has list whereas data structure doesn't.")
	}
	//create a new value whos type is the type of the elements of the slice(v).
	//this is the type that we will expect to be contained in the bencoded string.
	e := reflect.New(v.Type().Elem()).Elem()
	//loop and call decode for each bencoded element that you traverse
	//until you traverse an 'e' who is the first byte of a bencoded value.
	//after decoding, append the decoded value to the slice (v).
	for {
		if b, err = r.b.ReadByte(); err != nil {
			return err
		}
		if b == 'e' {
			break
		}
		err = r.b.UnreadByte()
		if err != nil {
			return err
		}
		err := decode(r, e)
		if err != nil {
			return err
		}
		v.Set(reflect.Append(v, e))
	}
	return nil
}

//v can be only of type reflect.Map.
func (r benReader) readBenDictMap(v reflect.Value) error {
	b, err := r.b.ReadByte()
	if err != nil {
		return err
	}
	if b != 'd' {
		return errors.New("Bencoded has dict whereas data structure doesn't.")
	}
	t := v.Type()
	keyType := t.Key()
	elemType := t.Elem()
	if keyType.Kind() != reflect.String {
		return errors.New("Maps should have keys of type string")
	}
	//create two zeroed values - the first represents the
	//map's key type and the second the map's value's type.
	//These are the types that we expect the bencoded string to have.
	keyVal := reflect.New(keyType).Elem()
	elemVal := reflect.New(elemType).Elem()
	//Iterate and start decoding values until you see an 'e' as the
	//first byte of a bencoded value.
	for {
		if b, err = r.b.ReadByte(); err != nil {
			return err
		}
		if b == 'e' {
			break
		}
		err = r.b.UnreadByte()
		if err != nil {
			return err
		}
		err = decode(r, keyVal)
		if err != nil {
			return err
		}
		err = decode(r, elemVal)
		if err != nil {
			return err
		}
		v.SetMapIndex(keyVal, elemVal)
	}
	return nil
}

//v can be only of type reflect.Struct.
func (r benReader) readBenDictStruct(v reflect.Value) error {
	b, err := r.b.ReadByte()
	if err != nil {
		return err
	}
	if b != 'd' {
		return errors.New("Bencoded has dict whereas data structure doesn't.")
	}
	t := v.Type()
	var benKey []byte
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
		if key != "" {
			fnames[key] = sf.Name
		} else {
			fnames[sf.Name] = sf.Name
		}
	}
	//decode loop
	for {
		if b, err = r.b.ReadByte(); err != nil {
			return err
		}
		if b == 'e' {
			break
		}
		err = r.b.UnreadByte()
		if err != nil {
			return err
		}
		benKey, err = r.readBenString()
		if err != nil {
			return err
		}
		sbenKey := string(benKey)
		//if there is a struct field with the same name as bencoded key,
		//decode the field value and delete the key from the nonOmit dict
		//if the key has not tag empty:"omit"
		if fname, ok := fnames[sbenKey]; ok {
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
				return err
			}
		} else {
			return errors.New("No struct field with name: " + sbenKey)
		}
	}
	//check if all mandatory fields were present in bencoded buffer.
	if len(nonOmit) != 0 {
		var s string
		for k := range nonOmit {
			s = s + "," + k
		}
		return errors.New("Some filled of the struct were not filled and they were not to be ommited: " + s)
	}
	return nil
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
		return errors.New("No case was satisfied in handleNilInterface. byte is: " + string(b))
	}
	if err != nil {
		return err
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
