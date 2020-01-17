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
			pc: i,
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
	assert.Equal(t, true, rq.full())
	var sendReady block
	//delete one block from the onFlights ones
	sendReady, ok = rq.deleteCompleted(block{
		pc: 8,
	})
	assert.Equal(t, ok, true)
	//ensure we will send the first block that exists in pending queue
	assert.Equal(t, sendReady, block{
		pc: maxOnFlight,
	})
	sendReady, ok = rq.deleteCompleted(block{
		pc: maxOnFlight + 5, //a piece that is not at onFlight
	})
	assert.Equal(t, false, ok)
	assert.Equal(t, sendReady, block{})
	ready, ok = rq.queue(block{
		pc: 1999,
	})
	assert.Equal(t, true, ok)
	assert.Equal(t, false, ready)
	blocks := rq.discardAll()
	assert.Equal(t, maxOnFlight+pendingLen, len(blocks))
	assert.Equal(t, true, rq.empty())
}
