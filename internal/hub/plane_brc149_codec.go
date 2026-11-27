package hub

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
)

// BRC-148 / BRC-149 Application-Layer Object Plane constants (1BSV Paper 006 §4.2 & Paper 010 §2, §8.2).
const (
	BRC148PlaneBase    uint16 = 0x1000
	BRC149HeaderBytes         = 36 // 32B TopicID + 4B uint32 BE ObjectLen
	PlaneFrameHdrBytes        = 44 // 8B uint64 BE FlowSeq + 32B TopicID + 4B uint32 BE ObjectLen
	MaxPlaneObjectLen  uint32 = 4 << 20
	maxPlaneSubs              = 256
)

// PlaneFrame is one numbered BRC-149 delivery record on Nektar's Application-Layer Object Plane.
// The BEEF bytes travel verbatim — never parsed, translated, or re-encoded by the relay.
type PlaneFrame struct {
	FlowSeq uint64
	TopicID [32]byte
	Topic   string // optional local label; mapped back from TopicID via subscriber election
	BEEF    []byte
}

// TopicIDOf computes the BRC-148 32-byte TopicID = SHA-256(UTF-8 topic name).
func TopicIDOf(topic string) [32]byte {
	return sha256.Sum256([]byte(topic))
}

// BandOf computes the BRC-148 multicast shard group index for topicID at widthBits (0..15):
//
//	IDX = 0x1000 + ((TopicID[0:4] as uint32 BE) >> (32 - widthBits))
//
// At widthBits == 0, the entire plane occupies 0x1000 (FF0E::B:1000).
func BandOf(topicID [32]byte, widthBits uint8) uint16 {
	if widthBits == 0 {
		return BRC148PlaneBase
	}
	if widthBits > 15 {
		widthBits = 15
	}
	word := binary.BigEndian.Uint32(topicID[0:4])
	shardIdx := uint16(word >> (32 - widthBits))
	return BRC148PlaneBase + shardIdx
}

// EncodeBRC149Record encodes a raw BRC-149 delivery record: [32B TopicID][4B uint32 BE len][BEEF].
func EncodeBRC149Record(topic string, beef []byte) ([]byte, error) {
	if len(beef) == 0 || uint32(len(beef)) > MaxPlaneObjectLen {
		return nil, fmt.Errorf("plane: object size %d out of bounds", len(beef))
	}
	out := make([]byte, BRC149HeaderBytes+len(beef))
	tid := TopicIDOf(topic)
	copy(out[0:32], tid[:])
	binary.BigEndian.PutUint32(out[32:36], uint32(len(beef)))
	copy(out[36:], beef)
	return out, nil
}

// DecodeBRC149Record reads one length-delimited BRC-149 delivery record with strict pre-allocation bounds.
func DecodeBRC149Record(r io.Reader, maxLen uint32) ([32]byte, []byte, error) {
	var tid [32]byte
	if maxLen == 0 || maxLen > MaxPlaneObjectLen {
		maxLen = MaxPlaneObjectLen
	}
	var hdr [BRC149HeaderBytes]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return tid, nil, err
	}
	copy(tid[:], hdr[0:32])
	objLen := binary.BigEndian.Uint32(hdr[32:36])
	if objLen == 0 || objLen > maxLen {
		return tid, nil, fmt.Errorf("plane: BRC-149 object length %d out of bounds (max %d)", objLen, maxLen)
	}
	beef := make([]byte, objLen)
	if _, err := io.ReadFull(r, beef); err != nil {
		return tid, nil, err
	}
	return tid, beef, nil
}

// EncodePlaneFrame encodes a numbered BRC-149 relay frame: [8B FlowSeq][32B TopicID][4B len][BEEF].
func EncodePlaneFrame(f PlaneFrame) ([]byte, error) {
	if len(f.BEEF) == 0 || uint32(len(f.BEEF)) > MaxPlaneObjectLen {
		return nil, fmt.Errorf("plane: object size %d out of bounds", len(f.BEEF))
	}
	tid := f.TopicID
	if tid == ([32]byte{}) && f.Topic != "" {
		tid = TopicIDOf(f.Topic)
	}
	out := make([]byte, PlaneFrameHdrBytes+len(f.BEEF))
	binary.BigEndian.PutUint64(out[0:8], f.FlowSeq)
	copy(out[8:40], tid[:])
	binary.BigEndian.PutUint32(out[40:44], uint32(len(f.BEEF)))
	copy(out[44:], f.BEEF)
	return out, nil
}

// DecodePlaneFrame decodes one numbered BRC-149 relay frame with strict bounds checking before allocating.
func DecodePlaneFrame(r io.Reader, maxLen uint32) (PlaneFrame, error) {
	if maxLen == 0 || maxLen > MaxPlaneObjectLen {
		maxLen = MaxPlaneObjectLen
	}
	var hdr [PlaneFrameHdrBytes]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return PlaneFrame{}, err
	}
	seq := binary.BigEndian.Uint64(hdr[0:8])
	var tid [32]byte
	copy(tid[:], hdr[8:40])
	objLen := binary.BigEndian.Uint32(hdr[40:44])
	if objLen == 0 || objLen > maxLen {
		return PlaneFrame{}, fmt.Errorf("plane: frame length %d out of bounds (max %d)", objLen, maxLen)
	}
	beef := make([]byte, objLen)
	if _, err := io.ReadFull(r, beef); err != nil {
		return PlaneFrame{}, err
	}
	return PlaneFrame{FlowSeq: seq, TopicID: tid, BEEF: beef}, nil
}
