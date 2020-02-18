package peer_wire

type Reserved [8]byte

func (r Reserved) SupportDHT() bool {
	return r[7]&0x1 != 0
}

func (r Reserved) SetDHT() {
	r[7] |= 0x01
}
