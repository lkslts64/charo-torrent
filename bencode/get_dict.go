package bencode

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
)

var ErrNoDict error = errors.New("data is not a bencoded dictionary")

//Get takes a bencoded dictionary as the first arg and returns
//the bencoded value of a key targetKey of the dictionary.
//Useful function for getting the valu of the info key of
//the meta info file. We assume that bencoded text is already
//parsed. Only UnkonwnValueError and ErrNoDict may be returned.
func Get(data []byte, targetKey string) (val []byte, ok bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, kk := r.(error); kk {
				err = fmt.Errorf("bencode get: %w", e)
			} else { //developer's mistake.
				panic(r)
			}
		}
	}()
	val, ok = get(data, targetKey)
	return
}

func get(data []byte, targetKey string) ([]byte, bool) {
	if data[0] != 'd' || data[len(data)-1] != 'e' {
		panic(ErrNoDict)
	}
	_data := data[1 : len(data)-1]
	_dataCopy := make([]byte, len(_data))
	copy(_dataCopy, _data)
	c := benConsumer{buf: bytes.NewBuffer(_data)}
	//stop if key is not found.
	for c.buf.Len() > 0 {
		start, end := c.read()
		key := make([]byte, end-start)
		copy(key, _dataCopy[start:end])
		s := ""
		if Decode(key, &s); s == targetKey {
			start, end := c.read()
			return _dataCopy[start:end], true
		}
		c.read()
	}
	return nil, false
}

type benConsumer struct {
	buf *bytes.Buffer
	off int
}

func (c *benConsumer) read() (start, end int) {
	start = c.off
	c.readBenElem()
	end = c.off
	return
}

func (c *benConsumer) readByte() (byte, error) {
	b, err := c.buf.ReadByte()
	if err != nil {
		return 0, err
	}
	c.off++
	return b, nil
}

func (c *benConsumer) readString(delim byte) (string, error) {
	s, err := c.buf.ReadString(delim)
	if err != nil {
		return "", err
	}
	c.off += len(s)
	return s, nil
}

func (c *benConsumer) next(n int) []byte {
	c.off += n
	return c.buf.Next(n)
}

func (c *benConsumer) readBenElem() bool {
	b, _ := c.readByte()
	if b == 'e' {
		return false
	}
	switch b {
	case 'l', 'd':
		for c.readBenElem() {
		}
	case 'i':
		c.readString('e')
	default:
		if b >= '0' && b <= '9' {
			s, _ := c.readString(':')
			strLen, _ := strconv.ParseInt(string(b)+s[:len(s)-1], 10, 64)
			c.next(int(strLen))
			break
		}
		panic(&UnknownValueError{string(b)})
	}
	return true

}
