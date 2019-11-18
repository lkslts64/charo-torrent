package torrent

type freqMap map[int64]int

func (f freqMap) add(n int64) {
	var count int
	var ok bool
	if count, ok = f[n]; !ok {
		f[n] = 1
	} else {
		f[n] = count + 1
	}
}

func (f freqMap) max() (max int64) {
	var maxFreq int
	for k, v := range f {
		if v > maxFreq {
			maxFreq = v
			max = k
		}
	}
	return
}
