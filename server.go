package osc

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"
	"unsafe"
)

// Common errors.
var (
	ErrAlreadyRunning = errors.New("server is already running")
	ErrNoDispatcher   = errors.New("no dispatcher defined")
	ErrPrematureClose = errors.New("server cannot be closed before calling Listen")
	ErrInvalidTypeTag = errors.New("invalid type tag")
)

// Server is an OSC server.
type Server struct {
	Address     string         // Address is the listening address.
	Listening   chan struct{}  // Listening is a channel used to indicate when the server is running.
	readTimeout time.Duration  // readTimeout is the timeout for reading from a connection.
	dispatcher  *OscDispatcher // Dispatcher that dispatches OSC packets/messages.
	running     bool           // Flag to store if the server is running or not.
	conn        *net.UDPConn   // conn is a UDP connection object.
}

// NewServer returns a new OSC Server.
func NewServer(addr string) (*Server, error) {
	return &Server{
		Address:     addr,
		Listening:   make(chan struct{}),
		readTimeout: 0,
		dispatcher:  NewOscDispatcher(),
	}, nil
}

// connect initializes the server's connection.
func (self *Server) connect() error {
	addr, err := net.ResolveUDPAddr("udp", self.Address)
	if err != nil {
		return err
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}

	dest := (*unsafe.Pointer)(unsafe.Pointer(&self.conn))
	if !atomic.CompareAndSwapPointer(dest, unsafe.Pointer(self.conn), unsafe.Pointer(conn)) {
		return errors.New("could not initialize connection")
	}

	return nil
}

// Close stops the OSC server and closes the connection.
func (self *Server) Close() error {
	if self.conn == nil {
		return nil
	}
	return self.conn.Close()
}

// AddMsgHandler registers a new message handler function for an OSC address. The handler
// is the function called for incoming Messages that match 'address'.
func (self *Server) AddMsgHandler(address string, handler HandlerFunc) error {
	return self.dispatcher.AddMsgHandler(address, handler)
}

// ListenAndServe retrieves incoming OSC packets and dispatches the retrieved OSC packets.
func (self *Server) ListenAndDispatch() error {
	if self.running {
		return ErrAlreadyRunning
	}

	if self.dispatcher == nil {
		return ErrNoDispatcher
	}

	if err := self.connect(); err != nil {
		return err
	}

	// Set read timeout
	if self.readTimeout != 0 {
		if err := self.conn.SetReadDeadline(time.Now().Add(self.readTimeout)); err != nil {
			return err
		}
	}

	self.running = true
	self.Listening <- struct{}{}

	for self.running {
		msg, err := self.readFromConnection()
		if err != nil {
			return err
		}
		self.dispatcher.Dispatch(msg)
	}

	return nil
}

// Listen causes the server to start listening for packets.
func (self *Server) Listen() error {
	if self.conn == nil {
		if err := self.connect(); err != nil {
			return err
		}
	}

	if self.running {
		return ErrAlreadyRunning
	}

	// Set read timeout
	if self.readTimeout != 0 {
		self.conn.SetReadDeadline(time.Now().Add(self.readTimeout))
	}

	self.running = true
	self.Listening <- struct{}{}

	return nil
}

// Send sends an OSC Bundle or an OSC Message.
func (self *Server) SendTo(addr net.Addr, packet Packet) (err error) {
	if self.conn == nil {
		return fmt.Errorf("connection not initialized")
	}
	data, err := packet.ToByteArray()
	if err != nil {
		self.conn.Close()
		return err
	}

	written, err := self.conn.WriteTo(data, addr)
	if err != nil {
		fmt.Println("could not write packet")
		self.conn.Close()
		return err
	}
	if written != len(data) {
		errmsg := "only wrote %d bytes of osc packet with length %d"
		return fmt.Errorf(errmsg, written, len(data))
	}

	return nil
}

// readFromConnection retrieves OSC packets.
func (self *Server) readFromConnection() (packet Packet, err error) {
	if self.conn == nil {
		return nil, fmt.Errorf("self.conn is nil")
	}
	data := make([]byte, 65535)
	var n, start int
	n, _, err = self.conn.ReadFromUDP(data)
	packet, err = self.readPacket(bufio.NewReader(bytes.NewBuffer(data)), &start, n)

	return packet, nil
}

// receivePacket receives an OSC packet from the given reader.
func (self *Server) readPacket(reader *bufio.Reader, start *int, end int) (packet Packet, err error) {
	var buf []byte
	buf, err = reader.Peek(1)
	if err != nil {
		return nil, err
	}

	// An OSC Message starts with a '/'
	if buf[0] == '/' {
		packet, err = self.readMessage(reader, start)
		if err != nil {
			return nil, err
		}
	} else if buf[0] == '#' { // An OSC bundle starts with a '#'
		packet, err = self.readBundle(reader, start, end)
		if err != nil {
			return nil, err
		}
	}

	return packet, nil
}

// readBundle reads an Bundle from reader.
func (self *Server) readBundle(reader *bufio.Reader, start *int, end int) (bundle *Bundle, err error) {
	// Read the '#bundle' OSC string
	var startTag string
	var n int
	startTag, n, err = readPaddedString(reader)
	if err != nil {
		return nil, err
	}
	*start += n

	if startTag != BundleTag {
		return nil, fmt.Errorf("Invalid bundle start tag: %s", startTag)
	}

	// Read the timetag
	var timeTag uint64
	if err := binary.Read(reader, binary.BigEndian, &timeTag); err != nil {
		return nil, err
	}
	*start += 8

	// Create a new bundle
	bundle = NewBundle(timetagToTime(timeTag))

	// Read until the end of the buffer
	for *start < end {
		// Read the size of the bundle element
		var length int32
		err = binary.Read(reader, binary.BigEndian, &length)
		*start += 4
		if err != nil {
			return nil, err
		}

		var packet Packet
		packet, err = self.readPacket(reader, start, end)
		if err != nil {
			return nil, err
		}
		bundle.Append(packet)
	}

	return bundle, nil
}

// readMessage reads one OSC Message from reader.
func (self *Server) readMessage(reader *bufio.Reader, start *int) (msg *Message, err error) {
	// First, read the OSC address
	var n int
	address, n, err := readPaddedString(reader)
	if err != nil {
		return nil, err
	}
	*start += n

	// Create a new message
	msg = &Message{address: address}

	// Read all arguments
	if err = self.readArguments(msg, reader, start); err != nil {
		return nil, err
	}

	return msg, nil
}

// readArguments reads all arguments from the reader and adds it to the OSC message.
func (self *Server) readArguments(msg *Message, reader *bufio.Reader, start *int) error {
	// Read the type tag string
	var n int
	typetags, n, err := readPaddedString(reader)
	if err != nil {
		return err
	}
	*start += n

	// If the typetag doesn't start with ',', it's not valid
	if typetags[0] != ',' {
		return ErrInvalidTypeTag
	}

	// Remove ',' from the type tag
	typetags = typetags[1:]

	for _, c := range typetags {
		switch c {
		default:
			return fmt.Errorf("Unsupported type tag: %c", c)

		// int32
		case 'i':
			var i int32
			if err = binary.Read(reader, binary.BigEndian, &i); err != nil {
				return err
			}
			*start += 4
			msg.Append(i)

		// int64
		case 'h':
			var i int64
			if err = binary.Read(reader, binary.BigEndian, &i); err != nil {
				return err
			}
			*start += 8
			msg.Append(i)

		// float32
		case 'f':
			var f float32
			if err = binary.Read(reader, binary.BigEndian, &f); err != nil {
				return err
			}
			*start += 4
			msg.Append(f)

		// float64/double
		case 'd':
			var d float64
			if err = binary.Read(reader, binary.BigEndian, &d); err != nil {
				return err
			}
			*start += 8
			msg.Append(d)

		// string
		case 's':
			// TODO: fix reading string value
			var s string
			if s, _, err = readPaddedString(reader); err != nil {
				return err
			}
			*start += len(s) + padBytesNeeded(len(s))
			msg.Append(s)

		// blob
		case 'b':
			var buf []byte
			var n int
			if buf, n, err = readBlob(reader); err != nil {
				return err
			}
			*start += n
			msg.Append(buf)

		// OSC Time Tag
		case 't':
			var tt uint64
			if err = binary.Read(reader, binary.BigEndian, &tt); err != nil {
				return nil
			}
			*start += 8
			msg.Append(Timetag(tt))

		// True
		case 'T':
			msg.Append(true)

		// False
		case 'F':
			msg.Append(false)
		}
	}

	return nil
}

// readBlob reads an OSC Blob from the blob byte array. Padding bytes are removed
// from the reader and not returned.
func readBlob(reader *bufio.Reader) (blob []byte, n int, err error) {
	// First, get the length
	var blobLen int
	if err = binary.Read(reader, binary.BigEndian, &blobLen); err != nil {
		return nil, 0, err
	}
	n = 4 + blobLen

	// Read the data
	blob = make([]byte, blobLen)
	if _, err = reader.Read(blob); err != nil {
		return nil, 0, err
	}

	// Remove the padding bytes
	numPadBytes := padBytesNeeded(blobLen)
	if numPadBytes > 0 {
		n += numPadBytes
		dummy := make([]byte, numPadBytes)
		if _, err = reader.Read(dummy); err != nil {
			return nil, 0, err
		}
	}

	return blob, n, nil
}