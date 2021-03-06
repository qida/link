package link

import (
	"net"
	"sync"
	"sync/atomic"
)

// Session.
type Session struct {
	id     uint64
	server *Server

	// About network
	conn   net.Conn
	writer PacketWriter
	reader PacketReader

	// About send and receive
	sendChan       chan Message
	sendPacketChan chan []byte
	sendBuff       []byte
	sendLock       *sync.Mutex
	messageHandler MessageHandler

	// About session close
	closeChan     chan int
	closeWait     *sync.WaitGroup
	closeFlag     int32
	closeCallback func(*Session)

	// Put your session state here.
	State interface{}
}

// Create a new session instance.
func NewSession(id uint64, conn net.Conn, protocol PacketProtocol, sendChanSize uint) *Session {
	return &Session{
		id:             id,
		conn:           conn,
		writer:         protocol.NewWriter(),
		reader:         protocol.NewReader(),
		sendChan:       make(chan Message, sendChanSize),
		sendPacketChan: make(chan []byte, sendChanSize),
		sendLock:       new(sync.Mutex),
		closeChan:      make(chan int),
		closeWait:      new(sync.WaitGroup),
		closeFlag:      -1,
	}
}

// Set session close callback.
func (session *Session) OnClose(callback func(*Session)) {
	session.closeCallback = callback
}

// Start the session's read write goroutines.
func (session *Session) Start() {
	if atomic.CompareAndSwapInt32(&session.closeFlag, -1, 0) {
		session.closeWait.Add(1)
		go session.writeLoop()

		session.closeWait.Add(1)
		go session.readLoop()
	} else {
		panic(SessionDuplicateStartError)
	}
}

// Loop and wait incoming requests.
func (session *Session) readLoop() {
	defer func() {
		session.closeWait.Done()
		session.Close()
	}()

	var (
		packet []byte
		err    error
	)

	for {
		packet, err = session.reader.ReadPacket(session.conn, packet)
		if err != nil {
			break
		}
		if session.messageHandler != nil {
			session.messageHandler.Handle(session, packet)
		}
	}
}

// Loop and transport responses.
func (session *Session) writeLoop() {
	defer func() {
		session.closeWait.Done()
		session.Close()
	}()
L:
	for {
		select {
		case message := <-session.sendChan:
			if err := session.SyncSend(message); err != nil {
				break L
			}
		case packet := <-session.sendPacketChan:
			if err := session.syncSendPacket(packet); err != nil {
				break L
			}
		case <-session.closeChan:
			break L
		}
	}
}

// Sync send a message.
func (session *Session) SyncSend(message Message) error {
	session.sendLock.Lock()
	defer session.sendLock.Unlock()

	size := message.RecommendPacketSize()

	packet := session.writer.BeginPacket(size, session.sendBuff)
	packet = message.AppendToPacket(packet)
	packet = session.writer.EndPacket(packet)

	session.sendBuff = packet

	err := session.writer.WritePacket(session.conn, packet)
	if err != nil {
		session.Close()
	}

	return err
}

// Sync send a packet.
func (session *Session) syncSendPacket(packet []byte) error {
	session.sendLock.Lock()
	defer session.sendLock.Unlock()

	err := session.writer.WritePacket(session.conn, packet)
	if err != nil {
		session.Close()
	}

	return err
}

// Get session id.
func (session *Session) Id() uint64 {
	return session.id
}

// Get local address.
func (session *Session) RawConn() net.Conn {
	return session.conn
}

// Set message handler function. A easy way to handle messages.
func (session *Session) OnMessage(callback func(*Session, []byte)) {
	session.messageHandler = messageHandlerFunc{callback}
}

// Set message handler. A complex but more powerful way to handle messages.
func (session *Session) SetMessageHandler(handler MessageHandler) {
	session.messageHandler = handler
}

// Close session and remove it from api server.
func (session *Session) Close() {
	// aways close the conn because session maybe closed before it start.
	session.conn.Close()

	if atomic.CompareAndSwapInt32(&session.closeFlag, 0, 1) {
		// if close session without this goroutine
		// deadlock will happen when session close itself.
		go func() {
			// notify write loop session closed
			close(session.closeChan)

			// wait for read loop and write lopp exit
			session.closeWait.Wait()

			// if this is a server side session
			// remove it from sessin list
			if session.server != nil {
				session.server.delSession(session)
			}

			// trigger the session close event
			if session.closeCallback != nil {
				session.closeCallback(session)
			}
		}()
	}
}

// Check session is closed or not.
func (session *Session) IsClosed() bool {
	return atomic.LoadInt32(&session.closeFlag) == 1
}

// Async send a message.
func (session *Session) Send(message Message) error {
	if atomic.LoadInt32(&session.closeFlag) != 0 {
		return SendToClosedError
	}

	select {
	case session.sendChan <- message:
		return nil
	default:
		session.Close()
		return BlockingError
	}
}

// Async send a packet.
func (session *Session) sendPacket(packet []byte) error {
	if atomic.LoadInt32(&session.closeFlag) != 0 {
		return SendToClosedError
	}

	select {
	case session.sendPacketChan <- packet:
		return nil
	default:
		session.Close()
		return BlockingError
	}
}
