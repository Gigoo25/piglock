package functions

import (
	"fmt"
	"sync"

	i2c "github.com/d2r2/go-i2c"
	logger "github.com/d2r2/go-logger"
)

// FRAM persistence layer for the analog hand position and coil pin polarity.
//
// Threat: power can be cut at any instant, including in the middle of a write.
// Defenses:
//   - Fixed 8-byte records guarded by a magic byte, a format version, a
//     monotonically increasing sequence number, and a CRC-8 over the payload.
//   - Two redundant slots (A and B) written alternately. A write only ever
//     touches the *inactive* slot, so a torn write can never corrupt the last
//     known-good record. On read we take the slot with a valid CRC and the
//     newest sequence number.
//
// Pin polarity is persisted because reverse ticking slips badly if it starts on
// the wrong coil pin after a restart (see ESPCLOCK4 notes).
const (
	FRAM_ADDRESS = 0x50 // Default I2C address for MB85RC256V

	framSlotA     = 0x0000 // first record slot
	framSlotB     = 0x0008 // second record slot
	framRecordLen = 8

	framMagic   = 0x5A // record marker
	framVersion = 0x01 // record format version

	framFlagPinB = 0x01 // flags bit0: current coil pin is pin2 (else pin1)
)

// FRAM represents a connection to an MB85RC256V FRAM chip.
type FRAM struct {
	mu         sync.Mutex
	device     *i2c.I2C
	seq        uint8 // sequence number of the last successfully written record
	activeSlot uint16
	primed     bool // true once we know which slot/seq is current
}

func init() {
	// Disable all I2C debug output.
	logger.ChangePackageLogLevel("i2c", logger.ErrorLevel)
}

// NewFRAM creates a new connection to the FRAM chip.
func NewFRAM(bus int) (*FRAM, error) {
	if err := Assert(bus >= 0, "i2c bus non-negative"); err != nil {
		return nil, err
	}
	device, err := i2c.NewI2C(FRAM_ADDRESS, bus)
	if err != nil {
		return nil, fmt.Errorf("error opening FRAM: %v", err)
	}
	return &FRAM{device: device, activeSlot: framSlotB}, nil
}

// Close closes the connection to the FRAM chip.
func (f *FRAM) Close() {
	if f.device != nil {
		f.device.Close()
	}
}

// crc8 computes a CRC-8/SMBUS (poly 0x07, init 0x00) over the buffer.
func crc8(data []byte) byte {
	var crc byte
	for _, b := range data {
		crc ^= b
		for range 8 {
			if crc&0x80 != 0 {
				crc = (crc << 1) ^ 0x07
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// encodeRecord builds an 8-byte record.
//
//	[0] magic   [1] version  [2] seq   [3] hour
//	[4] minute  [5] second   [6] flags [7] crc8(0..6)
func encodeRecord(seq uint8, hour, minute, second int, pinB bool) []byte {
	rec := make([]byte, framRecordLen)
	rec[0] = framMagic
	rec[1] = framVersion
	rec[2] = seq
	rec[3] = byte(hour)
	rec[4] = byte(minute)
	rec[5] = byte(second)
	if pinB {
		rec[6] = framFlagPinB
	}
	rec[7] = crc8(rec[:7])
	return rec
}

// decodeRecord validates a record and returns its fields.
func decodeRecord(rec []byte) (seq uint8, hour, minute, second int, pinB, ok bool) {
	if len(rec) < framRecordLen {
		return 0, 0, 0, 0, false, false
	}
	if rec[0] != framMagic || rec[1] != framVersion {
		return 0, 0, 0, 0, false, false
	}
	if crc8(rec[:7]) != rec[7] {
		return 0, 0, 0, 0, false, false
	}
	h, m, s := int(rec[3]), int(rec[4]), int(rec[5])
	if h < 0 || h > 11 || m < 0 || m > 59 || s < 0 || s > 59 {
		return 0, 0, 0, 0, false, false
	}
	return rec[2], h, m, s, rec[6]&framFlagPinB != 0, true
}

// readSlot reads and decodes one record slot.
func (f *FRAM) readSlot(addr uint16) (seq uint8, hour, minute, second int, pinB, ok bool) {
	if _, err := f.device.WriteBytes([]byte{byte(addr >> 8), byte(addr & 0xFF)}); err != nil {
		return 0, 0, 0, 0, false, false
	}
	buf := make([]byte, framRecordLen)
	if _, err := f.device.ReadBytes(buf); err != nil {
		return 0, 0, 0, 0, false, false
	}
	return decodeRecord(buf)
}

// newer reports whether sequence a is newer than b, accounting for 8-bit wrap.
func newer(a, b uint8) bool {
	return int8(a-b) > 0
}

// WriteState persists hand position and pin polarity to the inactive slot, then
// flips slots. A power loss during this write leaves the prior slot intact.
func (f *FRAM) WriteState(hour, minute, second int, pinB bool) error {
	if err := Assert(hour >= 0 && hour <= 11, "fram hour in range"); err != nil {
		return err
	}
	if err := Assert(minute >= 0 && minute <= 59 && second >= 0 && second <= 59, "fram min/sec in range"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.primed {
		f.primeLocked()
	}
	target := framSlotA
	if f.activeSlot == framSlotA {
		target = framSlotB
	}
	seq := f.seq + 1
	rec := encodeRecord(seq, hour, minute, second, pinB)

	payload := append([]byte{byte(target >> 8), byte(target & 0xFF)}, rec...)
	if _, err := f.device.WriteBytes(payload); err != nil {
		return fmt.Errorf("error writing record to FRAM: %v", err)
	}
	f.seq = seq
	f.activeSlot = uint16(target)
	f.primed = true
	return nil
}

// ReadState returns the newest valid persisted hand position and pin polarity.
func (f *FRAM) ReadState() (hour, minute, second int, pinB bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	seqA, hA, mA, sA, pA, okA := f.readSlot(framSlotA)
	seqB, hB, mB, sB, pB, okB := f.readSlot(framSlotB)

	switch {
	case okA && okB:
		if newer(seqA, seqB) {
			f.seq, f.activeSlot, f.primed = seqA, framSlotA, true
			return hA, mA, sA, pA, nil
		}
		f.seq, f.activeSlot, f.primed = seqB, framSlotB, true
		return hB, mB, sB, pB, nil
	case okA:
		f.seq, f.activeSlot, f.primed = seqA, framSlotA, true
		return hA, mA, sA, pA, nil
	case okB:
		f.seq, f.activeSlot, f.primed = seqB, framSlotB, true
		return hB, mB, sB, pB, nil
	default:
		return 0, 0, 0, false, fmt.Errorf("no valid time data in FRAM")
	}
}

// primeLocked refreshes seq/activeSlot from the chip. Caller holds f.mu.
func (f *FRAM) primeLocked() {
	seqA, _, _, _, _, okA := f.readSlot(framSlotA)
	seqB, _, _, _, _, okB := f.readSlot(framSlotB)
	switch {
	case okA && okB:
		if newer(seqA, seqB) {
			f.seq, f.activeSlot = seqA, framSlotA
		} else {
			f.seq, f.activeSlot = seqB, framSlotB
		}
	case okA:
		f.seq, f.activeSlot = seqA, framSlotA
	case okB:
		f.seq, f.activeSlot = seqB, framSlotB
	default:
		f.seq, f.activeSlot = 0, framSlotB
	}
	f.primed = true
}

// HasValidData reports whether either slot holds a valid record.
func (f *FRAM) HasValidData() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, _, _, _, _, okA := f.readSlot(framSlotA)
	_, _, _, _, _, okB := f.readSlot(framSlotB)
	return okA || okB
}
