package influxdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/boltdb/bolt"
)

// ShardGroup represents a group of shards created for a single time range.
type ShardGroup struct {
	ID        uint64    `json:"id,omitempty"`
	StartTime time.Time `json:"startTime,omitempty"`
	EndTime   time.Time `json:"endTime,omitempty"`
	Shards    []*Shard  `json:"shards,omitempty"`
}

// newShardGroup returns a new initialized ShardGroup instance.
func newShardGroup() *ShardGroup { return &ShardGroup{} }

// close closes all shards.
func (g *ShardGroup) close() {
	for _, sh := range g.Shards {
		_ = sh.close()
	}
}

// ShardBySeriesID returns the shard that a series is assigned to in the group.
func (g *ShardGroup) ShardBySeriesID(seriesID uint32) *Shard {
	return g.Shards[int(seriesID)%len(g.Shards)]
}

// Duration returns the duration between the shard group's start and end time.
func (g *ShardGroup) Duration() time.Duration { return g.EndTime.Sub(g.StartTime) }

// Contains return whether the shard group contains data for the time between min and max
func (g *ShardGroup) Contains(min, max time.Time) bool {
	return timeBetweenInclusive(g.StartTime, min, max) ||
		timeBetweenInclusive(g.EndTime, min, max) ||
		(g.StartTime.Before(min) && g.EndTime.After(max))
}

// dropSeries will delete all data with the seriesID
func (g *ShardGroup) dropSeries(seriesID uint32) error {
	for _, s := range g.Shards {
		err := s.dropSeries(seriesID)
		if err != nil {
			return err
		}
	}
	return nil
}

// Shard represents the logical storage for a given time range.
// The instance on a local server may contain the raw data in "store" if the
// shard is assigned to the server's data node id.
type Shard struct {
	ID          uint64   `json:"id,omitempty"`
	DataNodeIDs []uint64 `json:"nodeIDs,omitempty"` // owners

	index uint64        // highest replicated index
	store *bolt.DB      // underlying data store
	conn  MessagingConn // streaming connection to broker
}

// newShard returns a new initialized Shard instance.
func newShard() *Shard { return &Shard{} }

// open initializes and opens the shard's store.
func (s *Shard) open(path string, conn MessagingConn) error {
	// Return an error if the shard is already open.
	if s.store != nil {
		return errors.New("shard already open")
	}

	// Open store on shard.
	store, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return err
	}
	s.store = store

	// Initialize store.
	s.index = 0
	if err := s.store.Update(func(tx *bolt.Tx) error {
		_, _ = tx.CreateBucketIfNotExists([]byte("values"))

		// Find highest replicated index.
		b, _ := tx.CreateBucketIfNotExists([]byte("meta"))
		if buf := b.Get([]byte("index")); len(buf) > 0 {
			s.index = btou64(buf)
		}

		return nil
	}); err != nil {
		_ = s.close()
		return fmt.Errorf("init: %s", err)
	}

	// Open connection.
	if err := conn.Open(s.index, true); err != nil {
		_ = s.close()
		return fmt.Errorf("open shard conn: id=%d, idx=%d, err=%s", s.ID, s.index, err)
	}

	// Start importing from connection.
	go s.processor(conn)

	return nil
}

// close shuts down the shard's store.
func (s *Shard) close() error {
	if s.store != nil {
		_ = s.store.Close()
	}
	return nil
}

// HasDataNodeID return true if the data node owns the shard.
func (s *Shard) HasDataNodeID(id uint64) bool {
	for _, dataNodeID := range s.DataNodeIDs {
		if dataNodeID == id {
			return true
		}
	}
	return false
}

// readSeries reads encoded series data from a shard.
func (s *Shard) readSeries(seriesID uint32, timestamp int64) (values []byte, err error) {
	err = s.store.View(func(tx *bolt.Tx) error {
		// Find series bucket.
		b := tx.Bucket(u32tob(seriesID))
		if b == nil {
			return nil
		}

		// Retrieve encoded series data.
		values = b.Get(u64tob(uint64(timestamp)))
		return nil
	})
	return
}

// writeSeries writes series batch to a shard.
func (s *Shard) writeSeries(index uint64, batch []byte) error {
	return s.store.Update(func(tx *bolt.Tx) error {
		for {
			if pointHeaderSize > len(batch) {
				return ErrInvalidPointBuffer
			}
			seriesID, payloadLength, timestamp := unmarshalPointHeader(batch[:pointHeaderSize])
			batch = batch[pointHeaderSize:]

			if payloadLength > uint32(len(batch)) {
				return ErrInvalidPointBuffer
			}
			data := batch[:payloadLength]

			// Create a bucket for the series.
			b, err := tx.CreateBucketIfNotExists(u32tob(seriesID))
			if err != nil {
				return err
			}

			// Insert the values by timestamp.
			if err := b.Put(u64tob(uint64(timestamp)), data); err != nil {
				return err
			}

			// Push the buffer forward and check if we're done.
			batch = batch[payloadLength:]
			if len(batch) == 0 {
				break
			}
		}

		// Set index.
		if err := tx.Bucket([]byte("meta")).Put([]byte("index"), u64tob(index)); err != nil {
			return fmt.Errorf("write shard index: %s", err)
		}

		return nil
	})
}

func (s *Shard) dropSeries(seriesID uint32) error {
	if s.store == nil {
		return nil
	}
	return s.store.Update(func(tx *bolt.Tx) error {
		err := tx.DeleteBucket(u32tob(seriesID))
		if err != bolt.ErrBucketNotFound {
			return err
		}
		return nil
	})
}

// processor runs in a separate goroutine and processes all incoming broker messages.
func (s *Shard) processor(conn MessagingConn) {
	for {
		// Read incoming message.
		// Exit if the connection has been closed.
		m, ok := <-conn.C()
		if !ok {
			return
		}

		// Ignore any writes that are from an old index.
		if m.Index < s.index {
			continue
		}

		// Handle write series separately so we don't lock server during shard writes.
		switch m.Type {
		case writeRawSeriesMessageType:
			if err := s.writeSeries(m.Index, m.Data); err != nil {
				panic(fmt.Errorf("apply shard: id=%d, idx=%d, err=%s", s.ID, m.Index, err))
			}
		default:
			panic(fmt.Sprintf("invalid shard message type: %d", m.Type))
		}

		// Track last index.
		s.index = m.Index
	}
}

// Shards represents a list of shards.
type Shards []*Shard

// pointHeaderSize represents the size of a point header, in bytes.
const pointHeaderSize = 4 + 4 + 8 // seriesID + payload length + timestamp

// marshalPointHeader encodes a series id, payload length, timestamp, & flagset into a byte slice.
func marshalPointHeader(seriesID uint32, payloadLength uint32, timestamp int64) []byte {
	b := make([]byte, pointHeaderSize)
	binary.BigEndian.PutUint32(b[0:4], seriesID)
	binary.BigEndian.PutUint32(b[4:8], payloadLength)
	binary.BigEndian.PutUint64(b[8:16], uint64(timestamp))
	return b
}

// unmarshalPointHeader decodes a byte slice into a series id, timestamp & flagset.
func unmarshalPointHeader(b []byte) (seriesID uint32, payloadLength uint32, timestamp int64) {
	seriesID = binary.BigEndian.Uint32(b[0:4])
	payloadLength = binary.BigEndian.Uint32(b[4:8])
	timestamp = int64(binary.BigEndian.Uint64(b[8:16]))
	return
}

type uint8Slice []uint8

func (p uint8Slice) Len() int           { return len(p) }
func (p uint8Slice) Less(i, j int) bool { return p[i] < p[j] }
func (p uint8Slice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }
