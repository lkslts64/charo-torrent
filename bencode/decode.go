package bencode

import (
	"bytes"
	"errors"
	"reflect"
	"strconv"
)

func Decode(data []byte, v interface{}) error {
	e := reflect.ValueOf(v)
	if e.Type().Kind() != reflect.Ptr {
		return errors.New("v should have a pointer type")
	}
	r := benReader{bytes.NewBuffer(data)}
	err := decode(r, reflect.ValueOf(v))
	return err
}

/*func decode(r benReader, v reflect.Value) error {
	b, err := r.b.ReadByte()
	if err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}
	t := v.Type()
	switch b {
	case 'i':
		switch t.Kind() {
		case reflect.Bool:
			num, err := r.readBenBool()
			if err != nil {
				return err
			}
			v.SetBool(num)
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			num, err := r.readBenInt()
			if err != nil {
				return err
			}
			v.SetInt(num)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			num, err := r.readBenUint()
			if err != nil {
				return err
			}
			v.SetUint(num)
		default:
			return errors.New("bencoded buffer has an 'i' but the provieded data structure hasn't integer(bool,int,uint)  at the correspoing offset.")
		}
	case b >= 48 && b <= 57:
		err = r.b.UnreadByte()
		if err != nil {
			return err
		}
		bytes, err := r.readBenString()
		if err != nil {
			return err
		}
		switch t.Kind() {
		case reflect.String:
			v.SetString(string(bytes))
		case reflect.Slice && t.Elem().Kind() == reflect.Uint8:
			v.SetBytes(bytes)
		default:
			return errors.New("bencoded buffer has a string  but the provieded data structure hasn't a string or a []byte at the correspoing offset.")
		}
	case 'l':
		switch t.Kind() {
		case reflect.Slice && t.Elem().Kind() != reflect.Uint8:
			if v.Len() > 0 {
				return errors.New("A slice length of the data structures has not length zero.")
			}
		default:

		}

	case 'd':
	default:
	}

}
*/

func decode(r benReader, v reflect.Value) error {
	t := v.Type()
	switch v.Kind() {
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
		}
		r.readBenList()

	}
	return nil
}

type benReader struct {
	b *bytes.Buffer
}

//extracts the data from a bencoded string.
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

//extracts the data from a bencoded int and returns as an int
func (r benReader) readBenInt() (int64, error) {
	benInt, err := r.b.ReadString(byte('e'))
	if err != nil {
		return -1, err
	}
	if benInt[0] != 'i' {
		return -1, errors.New("Wanted integer but bencoded hasn't")
	}
	num, err := strconv.ParseInt(benInt[1:len(benInt)-1], 10, 64)
	if err != nil {
		return -1, err
	}
	return num, nil

}

//extracts the data from a bencoded int and returns it as an uint.
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

//extracts the data from a bencoded int and returns as a bool
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
	//until you traverse an 'e' who is acting as the first byte of a bencoded value.
	//after decoding, copy the decoded value to the slice (v).
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
		reflect.Append(v, e)
	}
}

//v is either a reflect.Map or a reflect.Struct.
func (r benReader) readBenDict(v reflect.Value) error {
	b, err := r.b.ReadByte()
	if err != nil {
		return err
	}
	if b != 'd' {
		return errors.New("Bencoded has dict whereas data structure doesn't.")
	}
	//create a new value whos type is the type of the elements of v.
	//this is the type that we will expect to be contained in the bencoded string.
	//e := reflect.New(v.Type().Elem()).Elem()
	t := v.Type()
	switch t.Kind() {
	case reflect.Struct:

		for i := 0; i < v.NumField(); i++ {
			fv := v.Field(i)
			sf := fv.Type().Field()
			err := decode(r, fv)

		}
		b, err = r.b.ReadByte()
		if err != nil {
			return err
		}
		if b != 'e' {
			return errors.New("No 'e' at end of dictionary")
		}
	case reflect.Map:
		keyType := t.Key()
		elemType := t.Elem()
		if keyType.Kind() != reflect.String { //or reflect.Slice containing uint8
			return errors.New("Maps should have keys of type string")
		}
		keyVal := reflect.New(keyType).Elem()
		elemVal := reflect.New(elemType).Elem()
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
	default:
		return errors.New("Expecting struct or map in readBenDict")
	}
}
