package db

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/couchbase/sync_gateway/base"
)

// A change sequence as reported externally in a _changes feed.
// Can support either integer- or vector clock-based sequences

// Most of the time the TriggerSeq is 0, but if a revision is being sent retroactively because
// the user got access to a channel, the TriggerSeq will be equal to the sequence of the change
// that gave the user access.

// SequenceID doesn't do any clock hash management - it's expected that hashing has already been done (if required)
// when the clock is set.
type SequenceID struct {
	SeqType          SequenceType       // Sequence Type (Int or Clock)
	TriggeredBy      uint64             // Int sequence: The sequence # that triggered this (0 if none)
	LowSeq           uint64             // Int sequence: Lowest contiguous sequence seen on the feed
	Seq              uint64             // Int sequence: The actual internal sequence
	Clock            base.SequenceClock // Clock sequence: Sequence (distributed index)
	TriggeredByClock base.SequenceClock // Clock sequence: Sequence (distributed index)
	vbNo             uint16             // Vbucket number for actual sequence
}

type SequenceType int

const (
	IntSequenceType = SequenceType(iota)
	ClockSequenceType
)

var MaxSequenceID = SequenceID{Seq: math.MaxUint64}

// Format sequence ID to send to clients.  Sequence IDs can be in one of the following formats:
//   Seq                    - simple sequence
//   TriggeredBy:Seq        - when TriggeredBy is non-zero, LowSeq is zero
//   LowSeq:TriggeredBy:Seq - when LowSeq is non-zero.
// When LowSeq is non-zero but TriggeredBy is zero, will appear as LowSeq::Seq.
// When LowSeq is non-zero but is greater than s.Seq (occurs when sending previously skipped sequences), ignore LowSeq.
func (s SequenceID) String() string {
	if s.SeqType == IntSequenceType {
		return s.intSeqToString()
	} else {
		return s.clockSeqToString()
	}
}

func (s SequenceID) intSeqToString() string {

	if s.LowSeq > 0 && s.LowSeq < s.Seq {
		if s.TriggeredBy > 0 {
			return fmt.Sprintf("%d:%d:%d", s.LowSeq, s.TriggeredBy, s.Seq)
		} else {
			return fmt.Sprintf("%d::%d", s.LowSeq, s.Seq)

		}
	} else if s.TriggeredBy > 0 {
		return fmt.Sprintf("%d:%d", s.TriggeredBy, s.Seq)
	} else {
		return strconv.FormatUint(s.Seq, 10)
	}
}

func (s SequenceID) clockSeqToString() string {

	if s.TriggeredByClock != nil {
		return fmt.Sprintf("%s:%d.%d", s.TriggeredByClock.GetHashedValue(), s.vbNo, s.Seq)
	} else {
		// If the clock has been set, return it's hashed value.  Otherwise, return
		// vb, sequence as vb.seq
		if s.Clock != nil {
			base.LogTo("DIndex+", "Clock is non nil, returning hashed value [%s]", s.Clock.GetHashedValue())
			return s.Clock.GetHashedValue()
		} else {
			base.LogTo("DIndex+", "Clock is nil, returning vbno, seq")
			return fmt.Sprintf("%d.%d", s.vbNo, s.Seq)
		}
	}
}

func (dbc *DatabaseContext) ParseSequenceID(str string) (s SequenceID, err error) {
	// If there's a sequence hasher defined, we're expecting clock-based sequences
	if dbc.sequenceHasher != nil {
		base.LogTo("DIndex+", "Handling changes as clock sequence...")
		return dbc.parseClockSequenceID(str)
	} else {
		base.LogTo("DIndex+", "Handling changes as int sequence...")
		return parseIntegerSequenceID(str)
	}
}

func parseIntegerSequenceID(str string) (s SequenceID, err error) {
	if str == "" {
		return SequenceID{}, nil
	}
	s.SeqType = IntSequenceType
	components := strings.Split(str, ":")
	if len(components) == 1 {
		// Just the internal sequence
		s.Seq, err = ParseIntSequenceComponent(components[0], false)
	} else if len(components) == 2 {
		// TriggeredBy and InternalSequence
		if s.TriggeredBy, err = ParseIntSequenceComponent(components[0], false); err != nil {
			return
		}
		if s.Seq, err = ParseIntSequenceComponent(components[1], false); err != nil {
			return
		}
	} else if len(components) == 3 {
		if s.LowSeq, err = ParseIntSequenceComponent(components[0], false); err != nil {
			return
		}
		if s.TriggeredBy, err = ParseIntSequenceComponent(components[1], true); err != nil {
			return
		}
		if s.Seq, err = ParseIntSequenceComponent(components[2], false); err != nil {
			return
		}
	} else {
		err = base.HTTPErrorf(400, "Invalid sequence")
	}

	if err != nil {
		err = base.HTTPErrorf(400, "Invalid sequence")
	}
	return
}

func (dbc *DatabaseContext) parseClockSequenceID(str string) (s SequenceID, err error) {

	if dbc.sequenceHasher == nil {
		return SequenceID{}, errors.New("No Sequence Hasher available to parse clock sequence ID")
	}

	if str == "" {
		return SequenceID{
			SeqType: ClockSequenceType,
			Clock:   base.NewSequenceClockImpl(),
		}, nil
	}

	s.SeqType = ClockSequenceType
	components := strings.Split(str, ":")
	if len(components) == 1 {
		// Just the standard clock
		if s.Clock, err = dbc.sequenceHasher.GetClock(components[0]); err != nil {
			return SequenceID{}, err
		}
	} else if len(components) == 2 {
		// TriggeredBy and Clock
		// TODO: parse triggered by
		if s.Clock, err = dbc.sequenceHasher.GetClock(components[0]); err != nil {
			return SequenceID{}, err
		}
	} else {
		err = base.HTTPErrorf(400, "Invalid sequence")
	}

	if err != nil {
		err = base.HTTPErrorf(400, "Invalid sequence")
	}
	return s, err
}

func ParseIntSequenceComponent(component string, allowEmpty bool) (uint64, error) {
	value := uint64(0)
	if allowEmpty && component == "" {
		return value, nil
	}
	value, err := strconv.ParseUint(component, 10, 64)
	return value, err

}

func (s SequenceID) MarshalJSON() ([]byte, error) {

	if s.SeqType == IntSequenceType {
		base.LogTo("DIndex+", "Marshalling int sequence...%v", s.String())
		if s.TriggeredBy > 0 || s.LowSeq > 0 {
			return []byte(fmt.Sprintf("\"%s\"", s.String())), nil
		} else {
			return []byte(strconv.FormatUint(s.Seq, 10)), nil
		}
	} else {
		base.LogTo("DIndex+", "Marshalling clock sequence...%v", s.clockSeqToString())
		return []byte(fmt.Sprintf("\"%s\"", s.clockSeqToString())), nil
	}

}

func (s *SequenceID) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var raw string
		err := json.Unmarshal(data, &raw)
		if err == nil {
			*s, err = parseIntegerSequenceID(string(raw))
		}
		return err
	} else {
		s.TriggeredBy = 0
		return json.Unmarshal(data, &s.Seq)
	}
}

func (s SequenceID) SafeSequence() uint64 {
	if s.LowSeq > 0 {
		return s.LowSeq
	} else {
		return s.Seq
	}
}

// The most significant value is TriggeredBy, unless it's zero, in which case use Seq.
// The tricky part is that "n" sorts after "n:m" for any nonzero m
func (s SequenceID) Before(s2 SequenceID) bool {

	// using SafeSequence for comparison, which takes the lower of LowSeq and Seq
	if s.TriggeredBy == s2.TriggeredBy {
		return s.SafeSequence() < s2.SafeSequence() // the simple case: untriggered, or triggered by same sequence
	} else if s.TriggeredBy == 0 {
		return s.SafeSequence() < s2.TriggeredBy // s2 triggered but not s
	} else if s2.TriggeredBy == 0 {
		return s.TriggeredBy <= s2.SafeSequence() // s triggered but not s2
	} else {
		return s.TriggeredBy < s2.TriggeredBy // both triggered, but by different sequences
	}
}
