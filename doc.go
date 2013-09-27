// Copyright (c) 2013, Daniel Morsing
// For more information, see the LICENSE file

package spdy

/*
 Internals documentation

 The goroutines, lifetimes and channels of this package is a bit involved,
 so here's a quick overview.

 Once a connection has been established, there are 3 main goroutines.

 - The session goroutine
 - The stream goroutine
 - The outFramer goroutine

 Session goroutine:

 The session goroutine keeps the internal state of the session and receives
 events from the client.  Whenever a stream is created, the session goroutine
 adds the stream to it's internal data structure and starts a stream goroutine.
 It also muxes events from the client connection to the appropriate stream
 goroutine. For example, it adjusts the window size, sends resets to the
 proper goroutine and fills the buffers of a stream when receiving data frames.

 interfaces:
	- closemsgch: send a message to this channel to request that the
	entire session shut down. This is usually used when there's a network
	or spdy protocol error.

	-closech: This channel is closed when the session is going away. If
	you intend to block for some event, receive from this channel in
	your select to be notified when the session is closing

	- finch: Send on this channel to request that a stream be removed
	from the session data structures. Note that it's the reposibility
	of the clients using this interface to send out reset messages and
	data frames with the fin flag set.

Stream goroutine:

For each request on a session, a stream goroutine is spawned. The handler
is run inside this goroutine and it's responsible for sending out the syn
stream frame, the data frames and closing frame.

interfaces:
	- reset: this channel is closed when the stream is either reset or
	the session is going away. If you're blocking on a select, listen
	to this channel to be notified.

	- recvData: this channel acts as a flag, telling the stream goroutine
	that there is new data to be read. Do a non-blocking send to this
	channel to notify the stream goroutine.

	- windowUpdate: this channel acts as a flag, telling the stream that
	there the send window has been updated and it can continue sending
	frames again. Do a non blocking select on this channel to notify
	the stream goroutine.

the outFramer goroutine:

The outFramer is the arbiter of access to the outgoing connection. Broadly
speaking, it implements a prioritized mutex, where the session is prioritized
highest and the streams are prioritized according to their SPDY session
priority.

The interface to this goroutine is the helper functions. They handle the
mutex locking and avoid allocation by using the internal buffers that the
outframer has.
*/
