package bencode

import (
	"fmt"
	"testing"
)

func TestEncode(t *testing.T) {
	got, err := Encode(struct {
		x int
		y int
	}{
		5,
		10,
	})
	fmt.Println(string(got), err)
}
