package torrent

import "go.uber.org/atomic"

//these are cancels that peers send to us but it was too late because
//we had already processed the request
var latecomerCancels atomic.Uint32
