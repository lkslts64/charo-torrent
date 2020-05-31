package peer_wire

type Reserved [8]byte

func (r Reserved) SupportDHT() bool {
	return r[7]&0x01 != 0
}

func (r *Reserved) SetDHT() {
	r[7] |= 0x01
}

func (r *Reserved) SetExtended() {
	r[5] |= 0x10
}

func (r Reserved) SupportExtended() bool {
	return r[5]&0x10 != 0
}
