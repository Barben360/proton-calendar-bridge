package cards_test

import (
	"sync"
	"testing"

	"github.com/ProtonMail/gopenpgp/v2/crypto"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/cards"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// Serve decrypts rows of one calendar from many requests at once with the
// same keyrings: under -race this pins that Open shares them safely.
func TestConcurrentOpensShareKeyRings(t *testing.T) {
	r := newRig(t)
	raws := [][]byte{
		r.add(t, protontest.Event{UID: "c1@pcb.test", Start: 1, End: 2, Card: fullCard}).Raw,
		r.add(t, protontest.Event{UID: "c2@pcb.test", Start: 1, End: 2, Card: fullCard, AddressKeyPacket: true}).Raw,
		r.add(t, protontest.Event{UID: "c3@pcb.test", Start: 1, End: 2, Card: fullCard, ForeignKey: true}).Raw,
	}
	addrs := map[string]*crypto.KeyRing{"addr1": r.addr}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if _, err := cards.Open(raws[(i+j)%len(raws)], r.calKR, addrs, r.own); err != nil {
					t.Error(err)
				}
			}
		}(i)
	}
	wg.Wait()
}
