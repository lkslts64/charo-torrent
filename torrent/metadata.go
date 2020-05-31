package torrent

import (
	"errors"
	"math/rand"
	"net"
	"sync"
)

//all utMetadata we are aware of. Usually it is only one but if different peers send us
//different metadata_sizes then we have to create multiple utMetadata structures.
type utMetadatas struct {
	mu sync.Mutex //guards following until we get info. After there is no need to lock/unlock
	m  map[int64]*utMetadata
	//set if we have verified the info bytes
	correctSize int64
}

func (uts *utMetadatas) reset(infoSize int) {
	uts.mu.Lock()
	defer uts.mu.Unlock()
	if ut, ok := uts.m[int64(infoSize)]; ok {
		ut.reset()
		return
	}
	panic("reset")
}

func (uts *utMetadatas) metadata(infoSize int64) *utMetadata {
	uts.mu.Lock()
	defer uts.mu.Unlock()
	if ut, ok := uts.m[int64(infoSize)]; ok {
		return ut
	}
	panic("metadata")
}

//returns the correct utmetadata structure or nil if we haven't verified info dict
func (uts *utMetadatas) correct() *utMetadata {
	uts.mu.Lock()
	defer uts.mu.Unlock()
	if uts.correctSize == 0 {
		return nil
	}
	if ut, ok := uts.m[uts.correctSize]; ok {
		return ut
	}
	return nil
}

func (uts *utMetadatas) setCorrect(infoSize int64) {
	uts.mu.Lock()
	defer uts.mu.Unlock()
	uts.correctSize = infoSize
}

func (uts *utMetadatas) add(msize int64) bool {
	uts.mu.Lock()
	defer uts.mu.Unlock()
	if msize < 0 || msize > 10000000 { //10MB,anacrolix pulled from his ass
		return false
	}
	if _, ok := uts.m[msize]; ok {
		return true
	}
	uts.m[msize] = newUtMetadata(int(msize))
	return true
	//TODO:add to contributor?
}

func (uts *utMetadatas) writeBlock(b []byte, i int, infoSize int, peerIP net.IP) (bool, error) {
	uts.mu.Lock()
	defer uts.mu.Unlock()
	if ut, ok := uts.m[int64(infoSize)]; ok {
		return ut.writeBlock(b, i, infoSize, peerIP)
	}
	panic("write Block uts")
}

func (uts *utMetadatas) readBlock(b []byte, i int) error {
	if len(uts.m) != 1 {
		panic("uts read")
	}
	for _, ut := range uts.m {
		return ut.readBlock(b, i)
	}
	panic("")
}

func (uts *utMetadatas) fillRequest(infoSize int, n int, exclude []int) []int {
	uts.mu.Lock()
	defer uts.mu.Unlock()
	if ut, ok := uts.m[int64(infoSize)]; ok {
		return ut.fillRequest(n, exclude)
	}
	panic("fill requests")
}

// utMetadata contains information for downloading/uploading the info dictionary
// BEP 9  https://www.bittorrent.org/beps/bep_0009.html()
type utMetadata struct {
	mu              sync.Mutex //guards following until we get info. After there is no need to lock/unlock
	infoBytes       []byte
	ownedInfoBlocks []bool
	contributors    []net.IP
	numPieces       int
	lastPieceLen    int
}

func (ut *utMetadata) size() int {
	return len(ut.infoBytes)
}

func newUtMetadata(infoSize int) *utMetadata {
	ut := &utMetadata{
		contributors: make([]net.IP, 0),
		infoBytes:    make([]byte, infoSize),
	}
	ut.numPieces = infoSize / metadataPieceSz
	if last := infoSize % metadataPieceSz; last != 0 {
		ut.lastPieceLen = last
		ut.numPieces++
	} else {
		ut.lastPieceLen = metadataPieceSz
	}
	ut.ownedInfoBlocks = make([]bool, ut.numPieces)
	return ut
}

func (ut *utMetadata) reset() {
	ut.contributors = make([]net.IP, 0)
	ut.ownedInfoBlocks = make([]bool, ut.numPieces)
}

func (ut *utMetadata) fillRequest(n int, exclude []int) []int {
	incompleteIndices := []int{}
	for i, v := range ut.ownedInfoBlocks {
		if !v && !contains(exclude, i) {
			incompleteIndices = append(incompleteIndices, i)
		}
	}
	rand.Shuffle(len(incompleteIndices), func(i, j int) {
		incompleteIndices[i], incompleteIndices[j] = incompleteIndices[j], incompleteIndices[i]
	})
	if len(incompleteIndices) > n {
		incompleteIndices = incompleteIndices[:n]
	}
	return incompleteIndices
}

func (ut *utMetadata) complete() bool {
	for _, v := range ut.ownedInfoBlocks {
		if !v {
			return false
		}
	}
	return true
}

func (ut *utMetadata) metadataPieceLength(i int) int {
	if i == ut.numPieces-1 {
		return ut.lastPieceLen
	}
	return metadataPieceSz
}

func (ut *utMetadata) writeBlock(b []byte, i int, totalSize int, peerIP net.IP) (bool, error) {
	if ut.ownedInfoBlocks[i] {
		return false, nil
	}
	if totalSize != len(ut.infoBytes) {
		panic("write block")
	}
	if i >= len(ut.ownedInfoBlocks) {
		return false, errors.New("write metadata piece: out of range")
	}
	if len(b) != ut.metadataPieceLength(i) {
		return false, errors.New("metadata: wrong piece legnth")
	}
	copy(ut.infoBytes[i*metadataPieceSz:], b)
	ut.ownedInfoBlocks[i] = true
	ut.contributors = append(ut.contributors, peerIP)
	return ut.complete(), nil
}

func (ut *utMetadata) readBlock(b []byte, i int) error {
	if i >= ut.numPieces {
		return errors.New("read metadata piece: out of range")
	}
	begin, end := i*metadataPieceSz, len(ut.infoBytes)
	if i < ut.numPieces-1 {
		end = (i + 1) * metadataPieceSz
	}
	copy(b, ut.infoBytes[begin:end])
	return nil
}
