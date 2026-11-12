package events

import "encoding/json"

// The Types of a card part sent to the sync endpoint (research A.2, B.1):
// bit 1 encrypted, bit 2 signed.
const (
	// PartSigned is a cleartext part: Data is the fragment, Signature a
	// detached signature over its exact bytes.
	PartSigned = 2
	// PartEncrypted is an encrypted and signed part: Data is the base64 of a
	// SEIPD data packet under a session key, Signature a detached signature
	// over the plaintext.
	PartEncrypted = 3
)

// Part is one card part as the sync endpoint takes it.
type Part struct {
	Type            int
	Data, Signature string
}

// PartList is a list of parts that encodes as [] when empty or nil, never
// null: Proton refuses a null part list (research A.2, proton-cal live).
type PartList []Part

// MarshalJSON encodes l, [] when nil.
func (l PartList) MarshalJSON() ([]byte, error) {
	if l == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]Part(l))
}

// WireEvent is the card-carrying half of a sync entry's Event (research
// A.2), with the endpoint's field names: the three part lists and the key
// packets. The key packets are the base64 of a PKESK of a session key to the
// primary calendar key; a create carries them, an update leaves them out
// (the row keeps its own), so they are omitted when empty. The part lists
// are always sent, [] when empty: an update replaces the whole event.
type WireEvent struct {
	SharedKeyPacket       string `json:",omitempty"`
	CalendarKeyPacket     string `json:",omitempty"`
	SharedEventContent    PartList
	CalendarEventContent  PartList
	AttendeesEventContent PartList
}
