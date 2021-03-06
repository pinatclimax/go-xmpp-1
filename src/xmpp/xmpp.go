package xmpp

import (
	"encoding/xml"
	"fmt"
	"log"
	"sync"
)

// Handles XMPP conversations over a Stream. Use NewClientXMPP or
// NewComponentXMPP to create and configure a XMPP instance.
// Close the conversation by closing the Out channel, the In channel will be
// closed when the remote server closes its stream.
type XMPP struct {

	// JID associated with the stream. Note: this may be negotiated with the
	// server during setup and so must be used for all messages.
	JID    JID
	stream *Stream

	// Channel of incoming messages. Values will be one of IQ, Message,
	// Presence, Error or error. Will be closed at the end when the stream is
	// closed or the stream's net connection dies.
	In chan interface{}

	// Channel of outgoing messages. Messages must be able to be marshaled by
	// the standard xml package, however you should try to send one of IQ,
	// Message or Presence.
	Out chan interface{}

	// Incoming stanza filters.
	filterLock   sync.Mutex
	nextFilterID FilterID
	filters      []filter
}

func newXMPP(jid JID, stream *Stream) *XMPP {
	x := &XMPP{
		JID:    jid,
		stream: stream,
		In:     make(chan interface{}),
		Out:    make(chan interface{}),
	}
	go x.sender()
	go x.receiver()
	return x
}

func (x *XMPP) SendRecv(iq *IQ) (*IQ, error) {

	fid, ch := x.AddFilter(IQResult(iq.ID))
	defer x.RemoveFilter(fid)

	x.Out <- iq

	stanza := <-ch
	reply, ok := stanza.(*IQ)
	if !ok {
		return nil, fmt.Errorf("Expected IQ, for %T", stanza)
	}
	return reply, nil
}

// Interface used to test if a stanza matches some application-defined
// conditions.
type Matcher interface {
	// Return true if the stanza, v, matches.
	Match(v interface{}) (match bool)
}

// Adapter to allow a plain func to be used as a Matcher.
type MatcherFunc func(v interface{}) bool

// Implement Matcher by calling the adapted func.
func (fn MatcherFunc) Match(v interface{}) bool {
	return fn(v)
}

// Uniquely identifies a stream filter. Used to remove a filter that's no
// longer needed.
type FilterID int64

// Implements the error interface for a FilterID.
func (fid FilterID) Error() string {
	return fmt.Sprintf("Invalid filter id: %d", fid)
}

type filter struct {
	id FilterID
	m  Matcher
	ch chan interface{}
}

// Add a filter that routes matching stanzas to the returned channel. A
// FilterID is also returned and can be pased to RemoveFilter to remove the
// filter again.
func (x *XMPP) AddFilter(m Matcher) (FilterID, chan interface{}) {

	// Protect against concurrent access.
	x.filterLock.Lock()
	defer x.filterLock.Unlock()

	// Allocate chan and id.
	ch := make(chan interface{})
	id := x.nextFilterID
	x.nextFilterID++

	// Insert at head of filters list.
	filters := make([]filter, len(x.filters)+1)
	filters[0] = filter{id, m, ch}
	copy(filters[1:], x.filters)
	x.filters = filters

	return id, ch
}

// Remove a filter previously added with AddFilter.
func (x *XMPP) RemoveFilter(id FilterID) error {

	// Protect against concurrent access.
	x.filterLock.Lock()
	defer x.filterLock.Unlock()

	// Find filter.
	for i, f := range x.filters {
		if f.id != id {
			continue
		}

		// Close the channel.
		close(f.ch)

		// Remove from list.
		filters := make([]filter, len(x.filters)-1)
		copy(filters, x.filters[:i])
		copy(filters[i:], x.filters[i+1:])
		x.filters = filters

		return nil
	}

	// Filter not found.
	return id
}

// Matcher to identify a <iq id="..." type="result" /> stanza with the given
// id.
func IQResult(id string) Matcher {
	return MatcherFunc(
		func(v interface{}) bool {
			iq, ok := v.(*IQ)
			if !ok {
				return false
			}
			if iq.ID != id {
				return false
			}
			return true
		},
	)
}

func (x *XMPP) sender() {

	// Send outgoing elements to the stream until the channel is closed.
	for v := range x.Out {
		x.stream.Send(v)
	}

	// Close the stream. Note: relies on common element name for all types of
	// XMPP connection.
	log.Println("Close XMPP stream")
	x.Close()
}

func (x *XMPP) receiver() {

	defer func() {
		log.Println("Close XMPP receiver")
		x.Close()
		close(x.In)
	}()

	for {
		start, err := x.stream.Next()
		if err != nil {
			x.In <- err
			return
		}

		var v interface{}
		switch start.Name.Local {
		case "error":
			v = &Error{}
		case "iq":
			v = &IQ{}
		case "message":
			v = &Message{}
		case "presence":
			v = &Presence{}
		default:
			log.Printf("Error. Unexected element: %T %v", start, start)
		}

		err = x.stream.Decode(v, start)
		if err != nil {
			log.Println("Error. Failed to decode element. ", err)
		}

		filtered := false
		for _, filter := range x.filters {
			if filter.m.Match(v) {
				filter.ch <- v
				filtered = true
			}
		}

		if !filtered {
			x.In <- v
		}
	}
}

func (x *XMPP) Close() {
	log.Println("Close XMPP")
	x.stream.SendEnd(&xml.EndElement{xml.Name{"stream", "stream"}})
}

// BUG(matt): Filter channels are not closed when the stream is closed.
