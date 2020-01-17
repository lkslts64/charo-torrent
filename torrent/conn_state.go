package torrent

type connState struct {
	amInterested bool
	amChoking    bool
	isInterested bool
	isChoking    bool
}

func newConnState() connState {
	return connState{
		amChoking: true,
		isChoking: true,
	}
}

func (cs *connState) canUpload() bool {
	return !cs.amChoking && cs.isInterested
}

func (cs *connState) canDownload() bool {
	return !cs.isChoking && cs.amInterested
}
