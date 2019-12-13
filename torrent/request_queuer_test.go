package torrent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRequestQueuer(t *testing.T) {
	pendingLen := 20
	rq := &requestQueuer{
		onFlight: make(map[block]struct{}),
		pending:  newBlockQueue(pendingLen),
	}
	var ok bool
	var ready bool
	for i := 0; i <= maxOnFlight+pendingLen; i++ {
		ready, ok = rq.queue(block{
			pc: uint32(i),
		})
		switch {
		case i < maxOnFlight:
			assert.Equal(t, true, ok)
			assert.Equal(t, true, ready)
		case i >= maxOnFlight && i < maxOnFlight+pendingLen:
			assert.Equal(t, true, ok)
			assert.Equal(t, false, ready)
		default:
			assert.Equal(t, false, ok)
			assert.Equal(t, false, ready)
		}
	}
	assert.Equal(t, pendingLen, len(rq.pending.blocks))
	assert.Equal(t, maxOnFlight, len(rq.onFlight))
	var sendReady block
	//delete one block from the onFlights ones
	sendReady, ok = rq.deleteCompleted(block{
		pc: uint32(8),
	})
	assert.Equal(t, ok, true)
	//ensure we will send the first block that exists in pending queue
	assert.Equal(t, sendReady, block{
		pc: 10,
	})
	sendReady, ok = rq.deleteCompleted(block{
		pc: uint32(20),
	})
	assert.Equal(t, false, ok)
	assert.Equal(t, sendReady, block{})
	ready, ok = rq.queue(block{
		pc: uint32(1999),
	})
	assert.Equal(t, true, ok)
	assert.Equal(t, false, ready)
	blocks := rq.discardAll()
	assert.Equal(t, maxOnFlight+pendingLen, len(blocks))
}
