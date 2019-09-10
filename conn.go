// Copyright 2013 The Gorilla WebSocket Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package websocket

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// Frame header byte 0 bits from Section 5.2 of RFC 6455
	finalBit = 1 << 7
	rsv1Bit  = 1 << 6
	rsv2Bit  = 1 << 5
	rsv3Bit  = 1 << 4

	// Frame header byte 1 bits from Section 5.2 of RFC 6455
	maskBit = 1 << 7

	maxFrameHeaderSize         = 2 + 8 + 4 // Fixed header + length + mask
	maxControlFramePayloadSize = 125

	writeWait = time.Second

	defaultReadBufferSize  = 4096
	defaultWriteBufferSize = 4096

	continuationFrame = 0
	noFrame           = -1
)

// Close codes defined in RFC 6455, section 11.7.
// 连接关闭状态码   websocket协议文档中定义
const (
	CloseNormalClosure           = 1000 // 表示一个正常的关闭，意味着连接建立的目标已经完成了。
	CloseGoingAway               = 1001 // 表示终端已经“走开”，例如服务器停机了或者在浏览器中离开了这个页面。
	CloseProtocolError           = 1002 // 表示终端由于协议错误中止了连接。
	CloseUnsupportedData         = 1003 // 表示终端由于收到了一个不支持的数据类型的数据（如终端只能怪理解文本数据，但是收到了一个二进制数据）从而关闭连接。
	CloseNoStatusReceived        = 1005 // 一个保留值并且不能被终端当做一个关闭帧的状态码。这个状态码是为了给上层应用表示当前没有状态码。
	CloseAbnormalClosure         = 1006 // 一个保留值并且不能被终端当做一个关闭帧的状态码。这个状态码是为了给上层应用表示连接被异常关闭如没有发送或者接受一个关闭帧这种场景的使用而设计的。
	CloseInvalidFramePayloadData = 1007 // 表示终端因为收到了类型不连续的消息（如非 UTF-8 编码的文本消息）导致的连接关闭。
	ClosePolicyViolation         = 1008 // 表示终端是因为收到了一个违反政策的消息导致的连接关闭。这是一个通用的状态码，可以在没有什么合适的状态码（如 1003 或者 1009）时或者可能需要隐藏关于政策的具体信息时返回。
	CloseMessageTooBig           = 1009 // 表示终端由于收到了一个太大的消息无法进行处理从而关闭连接。
	CloseMandatoryExtension      = 1010 // 表示终端（客户端）因为预期与服务端协商一个或者多个扩展，但是服务端在 WebSocket 握手中没有响应这个导致的关闭。需要的扩展清单应该出现在关闭帧的原因（reason）字段中。
	CloseInternalServerErr       = 1011 // 表示服务端因为遇到了一个意外的条件阻止它完成这个请求从而导致连接关闭。
	CloseServiceRestart          = 1012
	CloseTryAgainLater           = 1013
	CloseTLSHandshake            = 1015 //  是一个保留值，不能被终端设置到关闭帧的状态码中。这个状态码是用于上层应用来表示连接失败是因为 TLS 握手失败（如服务端证书没有被验证过）导致的关闭的。
)

// The message types are defined in RFC 6455, section 11.8.
const (
	// TextMessage denotes a text data message. The text message payload is
	// interpreted as UTF-8 encoded text data.
	// 文本消息类型  utf-8编码
	TextMessage = 1

	// BinaryMessage denotes a binary data message.
	// 二进制消息
	BinaryMessage = 2

	// CloseMessage denotes a close control message. The optional message
	// payload contains a numeric code and text. Use the FormatCloseMessage
	// function to format a close message payload.
	// 连接关闭消息
	CloseMessage = 8

	// PingMessage denotes a ping control message. The optional message payload
	// is UTF-8 encoded text.
	// ping消息
	PingMessage = 9

	// PongMessage denotes a pong control message. The optional message payload
	// is UTF-8 encoded text.
	// pong消息
	PongMessage = 10
)

// ErrCloseSent is returned when the application writes a message to the
// connection after sending a close message.
var ErrCloseSent = errors.New("websocket: close sent")

// ErrReadLimit is returned when reading a message that is larger than the
// read limit set for the connection.
var ErrReadLimit = errors.New("websocket: read limit exceeded")

// netError satisfies the net Error interface.
type netError struct {
	msg       string
	temporary bool
	timeout   bool
}

func (e *netError) Error() string   { return e.msg }
func (e *netError) Temporary() bool { return e.temporary }
func (e *netError) Timeout() bool   { return e.timeout }

// CloseError represents a close message.
type CloseError struct {
	// Code is defined in RFC 6455, section 11.7.
	Code int

	// Text is the optional text payload.
	Text string
}

func (e *CloseError) Error() string {
	s := []byte("websocket: close ")
	s = strconv.AppendInt(s, int64(e.Code), 10)
	switch e.Code {
	case CloseNormalClosure:
		s = append(s, " (normal)"...)
	case CloseGoingAway:
		s = append(s, " (going away)"...)
	case CloseProtocolError:
		s = append(s, " (protocol error)"...)
	case CloseUnsupportedData:
		s = append(s, " (unsupported data)"...)
	case CloseNoStatusReceived:
		s = append(s, " (no status)"...)
	case CloseAbnormalClosure:
		s = append(s, " (abnormal closure)"...)
	case CloseInvalidFramePayloadData:
		s = append(s, " (invalid payload data)"...)
	case ClosePolicyViolation:
		s = append(s, " (policy violation)"...)
	case CloseMessageTooBig:
		s = append(s, " (message too big)"...)
	case CloseMandatoryExtension:
		s = append(s, " (mandatory extension missing)"...)
	case CloseInternalServerErr:
		s = append(s, " (internal server error)"...)
	case CloseTLSHandshake:
		s = append(s, " (TLS handshake error)"...)
	}
	if e.Text != "" {
		s = append(s, ": "...)
		s = append(s, e.Text...)
	}
	return string(s)
}

// IsCloseError returns boolean indicating whether the error is a *CloseError
// with one of the specified codes.
func IsCloseError(err error, codes ...int) bool {
	if e, ok := err.(*CloseError); ok {
		for _, code := range codes {
			if e.Code == code {
				return true
			}
		}
	}
	return false
}

// IsUnexpectedCloseError returns boolean indicating whether the error is a
// *CloseError with a code not in the list of expected codes.
func IsUnexpectedCloseError(err error, expectedCodes ...int) bool {
	if e, ok := err.(*CloseError); ok {
		for _, code := range expectedCodes {
			if e.Code == code {
				return false
			}
		}
		return true
	}
	return false
}

var (
	errWriteTimeout        = &netError{msg: "websocket: write timeout", timeout: true, temporary: true}
	errUnexpectedEOF       = &CloseError{Code: CloseAbnormalClosure, Text: io.ErrUnexpectedEOF.Error()}
	errBadWriteOpCode      = errors.New("websocket: bad write message type")
	errWriteClosed         = errors.New("websocket: write closed")
	errInvalidControlFrame = errors.New("websocket: invalid control frame")
)

// 生成payload掩码
func newMaskKey() [4]byte {
	n := rand.Uint32()
	return [4]byte{byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}
}

func hideTempErr(err error) error {
	if e, ok := err.(net.Error); ok && e.Temporary() {
		err = &netError{msg: e.Error(), timeout: e.Timeout()}
	}
	return err
}

// 判断消息是否为控制帧
func isControl(frameType int) bool {
	return frameType == CloseMessage || frameType == PingMessage || frameType == PongMessage
}

// 判断消息是否为数据帧
func isData(frameType int) bool {
	return frameType == TextMessage || frameType == BinaryMessage
}

// 当前可用状态码   关闭状态码
var validReceivedCloseCodes = map[int]bool{
	// see http://www.iana.org/assignments/websocket/websocket.xhtml#close-code-number

	CloseNormalClosure:           true,
	CloseGoingAway:               true,
	CloseProtocolError:           true,
	CloseUnsupportedData:         true,
	CloseNoStatusReceived:        false,
	CloseAbnormalClosure:         false,
	CloseInvalidFramePayloadData: true,
	ClosePolicyViolation:         true,
	CloseMessageTooBig:           true,
	CloseMandatoryExtension:      true,
	CloseInternalServerErr:       true,
	CloseServiceRestart:          true,
	CloseTryAgainLater:           true,
	CloseTLSHandshake:            false,
}

// 判断接收到的关闭code是否合法
func isValidReceivedCloseCode(code int) bool {
	return validReceivedCloseCodes[code] || (code >= 3000 && code <= 4999)
}

// BufferPool represents a pool of buffers. The *sync.Pool type satisfies this
// interface.  The type of the value stored in a pool is not specified.
type BufferPool interface {
	// Get gets a value from the pool or returns nil if the pool is empty.
	Get() interface{}
	// Put adds a value to the pool.
	Put(interface{})
}

// writePoolData is the type added to the write buffer pool. This wrapper is
// used to prevent applications from peeking at and depending on the values
// added to the pool.
type writePoolData struct{ buf []byte }

// The Conn type represents a WebSocket connection.
type Conn struct {
	conn        net.Conn // 底层tcp连接实例  从http hijack 拿到
	isServer    bool     // mask相关  客户端发往服务端的帧payload需要掩码
	subprotocol string   // 连接实例所使用的子协议

	// Write fields
	mu            chan bool // used as mutex to protect write to conn 写操作保护
	writeBuf      []byte    // frame is constructed in this buffer. 写缓冲区
	writePool     BufferPool
	writeBufSize  int
	writeDeadline time.Time
	writer        io.WriteCloser // the current writer returned to the application
	isWriting     bool           // for best-effort concurrent write detection

	writeErrMu sync.Mutex // writeerr字段锁
	writeErr   error

	enableWriteCompression bool
	compressionLevel       int
	newCompressionWriter   func(io.WriteCloser, int) io.WriteCloser

	// Read fields
	// 返回给上层read方法
	reader  io.ReadCloser // the current reader returned to the application
	readErr error
	br      *bufio.Reader // 读
	// bytes remaining in current frame.
	// set setReadRemaining to safely update this value and prevent overflow
	readRemaining int64
	readFinal     bool  // true the current message has more frames.
	readLength    int64 // Message size.
	readLimit     int64 // Maximum message size.
	readMaskPos   int
	readMaskKey   [4]byte
	handlePong    func(string) error
	handlePing    func(string) error
	handleClose   func(int, string) error
	readErrCount  int
	// 实现read方法，调用br的read方法
	messageReader *messageReader // the current low-level reader

	readDecompress         bool // whether last read frame had RSV1 set
	newDecompressionReader func(io.Reader) io.ReadCloser
}

func newConn(conn net.Conn, isServer bool, readBufferSize, writeBufferSize int, writeBufferPool BufferPool, br *bufio.Reader, writeBuf []byte) *Conn {
	// 如果hajacked的buffer reader为nil
	if br == nil {
		// 如果指定了websocket连接的readbuffersize
		if readBufferSize == 0 {
			readBufferSize = defaultReadBufferSize
		} else if readBufferSize < maxControlFramePayloadSize {
			// must be large enough for control frame
			// readbuffer最少能够存的下一个完整的控制帧
			readBufferSize = maxControlFramePayloadSize
		}
		// 为连接创建一个bufferreader
		br = bufio.NewReaderSize(conn, readBufferSize)
	}

	if writeBufferSize <= 0 {
		writeBufferSize = defaultWriteBufferSize
	}
	writeBufferSize += maxFrameHeaderSize
	// 写缓冲区
	if writeBuf == nil && writeBufferPool == nil {
		writeBuf = make([]byte, writeBufferSize)
	}

	// 写控制  每帧数据写完后会发送 mu<- true 释放控制权
	mu := make(chan bool, 1)
	mu <- true
	c := &Conn{
		isServer:               isServer,
		br:                     br,
		conn:                   conn,
		mu:                     mu,
		readFinal:              true,
		writeBuf:               writeBuf,
		writePool:              writeBufferPool,
		writeBufSize:           writeBufferSize,
		enableWriteCompression: true,
		compressionLevel:       defaultCompressionLevel,
	}
	c.SetCloseHandler(nil)
	c.SetPingHandler(nil)
	c.SetPongHandler(nil)
	return c
}

// setReadRemaining tracks the number of bytes remaining on the connection. If n
// overflows, an ErrReadLimit is returned.
func (c *Conn) setReadRemaining(n int64) error {
	if n < 0 {
		return ErrReadLimit
	}

	c.readRemaining = n
	return nil
}

// Subprotocol returns the negotiated protocol for the connection.
func (c *Conn) Subprotocol() string {
	return c.subprotocol
}

// Close closes the underlying network connection without sending or waiting
// for a close message.
func (c *Conn) Close() error {
	return c.conn.Close()
}

// LocalAddr returns the local network address.
func (c *Conn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

// RemoteAddr returns the remote network address.
func (c *Conn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// Write methods

func (c *Conn) writeFatal(err error) error {
	err = hideTempErr(err)
	c.writeErrMu.Lock()
	if c.writeErr == nil {
		c.writeErr = err
	}
	c.writeErrMu.Unlock()
	return err
}

func (c *Conn) read(n int) ([]byte, error) {
	p, err := c.br.Peek(n)
	if err == io.EOF {
		err = errUnexpectedEOF
	}
	// 读完n个字节后将这n字节数据从读缓冲区中丢弃
	c.br.Discard(len(p))
	return p, err
}

// 向对端写数据
func (c *Conn) write(frameType int, deadline time.Time, buf0, buf1 []byte) error {
	// 获取写锁
	<-c.mu
	// 退出释放写锁
	defer func() { c.mu <- true }()

	// 获取上次写结果，如果有错误直接返回
	c.writeErrMu.Lock()
	err := c.writeErr
	c.writeErrMu.Unlock()
	if err != nil {
		return err
	}

	// 设置写超时，写数据
	c.conn.SetWriteDeadline(deadline)
	if len(buf1) == 0 {
		_, err = c.conn.Write(buf0)
	} else {
		err = c.writeBufs(buf0, buf1)
	}
	if err != nil { // 写错误，设置writeErr
		return c.writeFatal(err)
	}
	// 设置关闭错误
	if frameType == CloseMessage {
		c.writeFatal(ErrCloseSent)
	}
	return nil
}

// WriteControl writes a control message with the given deadline. The allowed
// message types are CloseMessage, PingMessage and PongMessage.
func (c *Conn) WriteControl(messageType int, data []byte, deadline time.Time) error {
	if !isControl(messageType) {
		return errBadWriteOpCode
	}
	// 判断数据是否超过控制帧的payload
	if len(data) > maxControlFramePayloadSize {
		return errInvalidControlFrame
	}

	// 控制帧第一个字节   10000000 | msgtype
	//close： 10000000 | 0x8 = 10001000
	// ping: 10000000 | 0x9 = 10001001
	// pong: 10000000 | 0xA = 10001010
	b0 := byte(messageType) | finalBit
	// 第二个字节  低7位写入payload长度
	b1 := byte(len(data))
	// 服务端发向客户端的数据不需要掩码
	if !c.isServer {
		b1 |= maskBit
	}

	buf := make([]byte, 0, maxFrameHeaderSize+maxControlFramePayloadSize)
	buf = append(buf, b0, b1)

	if c.isServer {
		// 服务端发向客户端  不需要掩码 直接拷贝payload
		buf = append(buf, data...)
	} else {
		// 客户端发向服务端需要掩码
		key := newMaskKey()
		buf = append(buf, key[:]...)
		buf = append(buf, data...)
		// 控制帧带掩码 6个字节 只异或payload
		maskBytes(key, 0, buf[6:])
	}

	d := time.Hour * 1000
	if !deadline.IsZero() {
		d = deadline.Sub(time.Now()) // 超时时间
		if d < 0 {
			return errWriteTimeout
		}
	}

	// 等待d，如果没能从mu chan中读到数据说明在超时时间内没能拿到写控制权，返回写超时
	timer := time.NewTimer(d)
	select {
	case <-c.mu:
		timer.Stop()
	case <-timer.C:
		return errWriteTimeout
	}
	// 函数退出时释放对写操作的控制
	defer func() { c.mu <- true }()

	// 获取上次写操作的错误码？？
	c.writeErrMu.Lock()
	err := c.writeErr
	c.writeErrMu.Unlock()
	if err != nil {
		return err
	}
	// 设置写超时 并向客户端写数据
	c.conn.SetWriteDeadline(deadline)
	_, err = c.conn.Write(buf)
	if err != nil {
		return c.writeFatal(err)
	}
	if messageType == CloseMessage {
		c.writeFatal(ErrCloseSent)
	}
	return err
}

// beginMessage prepares a connection and message writer for a new message.
func (c *Conn) beginMessage(mw *messageWriter, messageType int) error {
	// Close previous writer if not already closed by the application. It's
	// probably better to return an error in this situation, but we cannot
	// change this without breaking existing applications.
	if c.writer != nil {
		c.writer.Close()
		c.writer = nil
	}
	// 判断消息类型是否合法
	if !isControl(messageType) && !isData(messageType) {
		return errBadWriteOpCode
	}

	// 获取上一次写错误
	c.writeErrMu.Lock()
	err := c.writeErr
	c.writeErrMu.Unlock()
	if err != nil {
		return err
	}

	mw.c = c
	mw.frameType = messageType
	mw.pos = maxFrameHeaderSize

	// 如果upgrader初始化时指定了writepool 且没有给writebuf分配空间
	if c.writeBuf == nil {
		wpd, ok := c.writePool.Get().(writePoolData)
		if ok {
			c.writeBuf = wpd.buf
		} else {
			c.writeBuf = make([]byte, c.writeBufSize)
		}
	}
	return nil
}

// NextWriter returns a writer for the next message to send. The writer's Close
// method flushes the complete message to the network.
//
// There can be at most one open writer on a connection. NextWriter closes the
// previous writer if the application has not already done so.
//
// All message types (TextMessage, BinaryMessage, CloseMessage, PingMessage and
// PongMessage) are supported.
func (c *Conn) NextWriter(messageType int) (io.WriteCloser, error) {
	var mw messageWriter
	if err := c.beginMessage(&mw, messageType); err != nil {
		return nil, err
	}
	c.writer = &mw
	if c.newCompressionWriter != nil && c.enableWriteCompression && isData(messageType) {
		w := c.newCompressionWriter(c.writer, c.compressionLevel)
		mw.compress = true
		c.writer = w
	}
	return c.writer, nil
}

// 上层写方法
type messageWriter struct {
	c        *Conn
	compress bool // whether next call to flushFrame should set RSV1
	// writebuf中数据长度
	pos int // end of data in writeBuf.
	// 当前帧类型
	frameType int // type of the current frame.
	err       error
}

func (w *messageWriter) endMessage(err error) error {
	if w.err != nil {
		return err
	}
	c := w.c
	w.err = err
	c.writer = nil
	if c.writePool != nil {
		c.writePool.Put(writePoolData{buf: c.writeBuf})
		c.writeBuf = nil
	}
	return err
}

// flushFrame writes buffered data and extra as a frame to the network. The
// final argument indicates that this is the last frame in the message.
func (w *messageWriter) flushFrame(final bool, extra []byte) error {
	c := w.c
	// payload长度
	length := w.pos - maxFrameHeaderSize + len(extra)

	// Check for invalid control frames.
	// 控制帧 final==true length<125
	if isControl(w.frameType) &&
		(!final || length > maxControlFramePayloadSize) {
		return w.endMessage(errInvalidControlFrame)
	}

	b0 := byte(w.frameType)
	if final {
		b0 |= finalBit
	}
	if w.compress {
		b0 |= rsv1Bit
	}
	w.compress = false

	b1 := byte(0)
	// 客户端发向服务端 带掩码
	if !c.isServer {
		b1 |= maskBit
	}

	// 帧头长度有可能小于2+8+4，如果是服务端发向客户端的数据则不需要掩码，framePos向后移4个字节
	// 使用framePos可以使帧头和payload数据紧接在一起
	// Assume that the frame starts at beginning of c.writeBuf.
	framePos := 0
	if c.isServer {
		// Adjust up if mask not included in the header.
		framePos = 4
	}

	// payload长度大于65535，则payload长度需要使用8bytes（无符号整型）来存储，如果是服务端发向客户端
	// 则不需要掩码，使用只需要使用writeBuf中前maxFrameHeaderSize字节的后(maxFrameHeaderSize-4)个字节
	// 其他长度以此类推
	switch {
	case length >= 65536:
		c.writeBuf[framePos] = b0
		c.writeBuf[framePos+1] = b1 | 127
		binary.BigEndian.PutUint64(c.writeBuf[framePos+2:], uint64(length))
	case length > 125:
		framePos += 6
		c.writeBuf[framePos] = b0
		c.writeBuf[framePos+1] = b1 | 126
		binary.BigEndian.PutUint16(c.writeBuf[framePos+2:], uint16(length))
	default:
		framePos += 8
		c.writeBuf[framePos] = b0
		c.writeBuf[framePos+1] = b1 | byte(length)
	}

	if !c.isServer {
		key := newMaskKey()
		// 如果需要掩码   掩码位置位于payload前4个字节
		copy(c.writeBuf[maxFrameHeaderSize-4:], key[:])
		// 对payload数据进行异或计算
		maskBytes(key, 0, c.writeBuf[maxFrameHeaderSize:w.pos])
		// 客户端发向服务端，帧数据长度大于buf长度 报错     客户端分片数据长度不能大于buf长度
		if len(extra) > 0 {
			return w.endMessage(c.writeFatal(errors.New("websocket: internal error, extra used in client mode")))
		}
	}

	// Write the buffers to the connection with best-effort detection of
	// concurrent writes. See the concurrency section in the package
	// documentation for more info.

	if c.isWriting {
		panic("concurrent write to websocket connection")
	}
	c.isWriting = true

	err := c.write(w.frameType, c.writeDeadline, c.writeBuf[framePos:w.pos], extra)

	if !c.isWriting {
		panic("concurrent write to websocket connection")
	}
	c.isWriting = false

	if err != nil {
		return w.endMessage(err)
	}
	// 数据片段最后一帧写完
	if final {
		w.endMessage(errWriteClosed) // ???
		return nil
	}

	// Setup for next frame.
	// 数据片段还有后续帧
	w.pos = maxFrameHeaderSize
	w.frameType = continuationFrame
	return nil
}

func (w *messageWriter) ncopy(max int) (int, error) {
	n := len(w.c.writeBuf) - w.pos
	// writebuf填充满后发送给对端
	if n <= 0 {
		if err := w.flushFrame(false, nil); err != nil {
			return 0, err
		}
		n = len(w.c.writeBuf) - w.pos
	}
	// 返回writebuf中剩余的空间长度
	if n > max {
		n = max
	}
	return n, nil
}

func (w *messageWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}

	if len(p) > 2*len(w.c.writeBuf) && w.c.isServer {
		// Don't buffer large messages.
		err := w.flushFrame(false, p)
		if err != nil {
			return 0, err
		}
		return len(p), nil
	}

	nn := len(p)
	// 向writebuf中copy数据，copy满后ncopy中会判断，并将writebuf中的数据作为一帧数据发送出去
	for len(p) > 0 {
		n, err := w.ncopy(len(p))
		if err != nil {
			return 0, err
		}
		copy(w.c.writeBuf[w.pos:], p[:n])
		w.pos += n
		p = p[n:]
	}
	// p中的数据copy完后，writebuf中的数据不会在Write函数返回时当做一帧数据被发送出去，
	// 此时外部可以继续调用Write方法，继续向writebuf中写入数据，当writebuf被填满时，writebuf
	// 将作为一帧被发送出去，或者外部调用Close()方法，Close方法会将writebuf中剩余的数据当做
	// 最后一帧数据发送出去。
	return nn, nil
}

func (w *messageWriter) WriteString(p string) (int, error) {
	if w.err != nil {
		return 0, w.err
	}

	nn := len(p)
	for len(p) > 0 {
		n, err := w.ncopy(len(p))
		if err != nil {
			return 0, err
		}
		copy(w.c.writeBuf[w.pos:], p[:n])
		w.pos += n
		p = p[n:]
	}
	return nn, nil
}

func (w *messageWriter) ReadFrom(r io.Reader) (nn int64, err error) {
	if w.err != nil {
		return 0, w.err
	}
	for {
		if w.pos == len(w.c.writeBuf) {
			err = w.flushFrame(false, nil)
			if err != nil {
				break
			}
		}
		var n int
		n, err = r.Read(w.c.writeBuf[w.pos:])
		w.pos += n
		nn += int64(n)
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			break
		}
	}
	return nn, err
}

func (w *messageWriter) Close() error {
	if w.err != nil {
		return w.err
	}
	// 将writebuf中剩余的数据最为最后一帧发送出去
	return w.flushFrame(true, nil)
}

// WritePreparedMessage writes prepared message into connection.
func (c *Conn) WritePreparedMessage(pm *PreparedMessage) error {
	frameType, frameData, err := pm.frame(prepareKey{
		isServer:         c.isServer,
		compress:         c.newCompressionWriter != nil && c.enableWriteCompression && isData(pm.messageType),
		compressionLevel: c.compressionLevel,
	})
	if err != nil {
		return err
	}
	if c.isWriting {
		panic("concurrent write to websocket connection")
	}
	c.isWriting = true
	err = c.write(frameType, c.writeDeadline, frameData, nil)
	if !c.isWriting {
		panic("concurrent write to websocket connection")
	}
	c.isWriting = false
	return err
}

// WriteMessage is a helper method for getting a writer using NextWriter,
// writing the message and closing the writer.
func (c *Conn) WriteMessage(messageType int, data []byte) error {
	// 服务端发向客户端 单帧
	if c.isServer && (c.newCompressionWriter == nil || !c.enableWriteCompression) {
		// Fast path with no allocations and single frame.

		var mw messageWriter
		if err := c.beginMessage(&mw, messageType); err != nil {
			return err
		}
		// writeBuf default 4096 + 2+8+4
		n := copy(c.writeBuf[mw.pos:], data)
		mw.pos += n
		data = data[n:]
		return mw.flushFrame(true, data)
	}

	w, err := c.NextWriter(messageType)
	if err != nil {
		return err
	}
	// 调用meeeageWriter的Write方法
	if _, err = w.Write(data); err != nil {
		return err
	}
	// 调用meeeageWriter的Close方法
	return w.Close()
}

// SetWriteDeadline sets the write deadline on the underlying network
// connection. After a write has timed out, the websocket state is corrupt and
// all future writes will return an error. A zero value for t means writes will
// not time out.
func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.writeDeadline = t
	return nil
}

// Read methods

func (c *Conn) advanceFrame() (int, error) {
	// 1. Skip remainder of previous frame.
	// 丢弃上一帧剩余的数据
	if c.readRemaining > 0 {
		if _, err := io.CopyN(ioutil.Discard, c.br, c.readRemaining); err != nil {
			return noFrame, err
		}
	}

	// 2. Read and parse first two bytes of frame header.
	// 读取帧的前2个字节
	p, err := c.read(2)
	if err != nil {
		return noFrame, err
	}
	//  网络序 高位在左边  判断是否为消息的最后一个片段
	// FIN为0：不是最后一帧
	final := p[0]&finalBit != 0
	// opcode 4bits
	frameType := int(p[0] & 0xf)
	// “有效负载数据”是否添加掩码  如果mask = 1 则掩码键值存在与masking-key中
	// 这个一般用于解码“有效负载数据”。所有的从客户端发送到服务端的帧都需要设置这个bit位为1
	mask := p[1]&maskBit != 0
	// 以字节为单位的“有效负载数据”长度，如果值为0-125，那么就表示负载数据的长度。如果是126，
	// 那么接下来的2个bytes解释为16bit的无符号整形作为负载数据的长度。如果是127，那么接下来
	// 的8个bytes解释为一个64bit的无符号整形（最高位的bit必须为0）作为负载数据的长度
	c.setReadRemaining(int64(p[1] & 0x7f))

	c.readDecompress = false
	if c.newDecompressionReader != nil && (p[0]&rsv1Bit) != 0 {
		c.readDecompress = true
		p[0] &^= rsv1Bit
	}
	// RSV1，RSV2，RSV3: 每个1 bit  必须设置为0，除非扩展了非0值含义的扩展
	if rsv := p[0] & (rsv1Bit | rsv2Bit | rsv3Bit); rsv != 0 {
		return noFrame, c.handleProtocolError("unexpected reserved bits 0x" + strconv.FormatInt(int64(rsv), 16))
	}

	switch frameType {
	//所有的控制帧必须有一个126字节或者更小的负载长度，并且不能被分片
	case CloseMessage, PingMessage, PongMessage:
		if c.readRemaining > maxControlFramePayloadSize {
			return noFrame, c.handleProtocolError("control frame length > 125")
		}
		if !final {
			return noFrame, c.handleProtocolError("control frame not final")
		}
	// 数据帧（例如非控制帧）的定义是操作码的最高位值为0。当前定义的数据帧操作吗包含0x1（文本）、0x2（二进制）。
	// 操作码0x3-0x7是被保留作为非控制帧的操作码。
	case TextMessage, BinaryMessage:
		// 如果readFinal == false 进入此分支 说明出错
		if !c.readFinal {
			return noFrame, c.handleProtocolError("message start before final message frame")
		}
		c.readFinal = final
	case continuationFrame:
		// readFinal == false 非最后一帧 只能进入此分支
		// 如果为最后一个片段 则会更新readFinal为true
		if c.readFinal {
			return noFrame, c.handleProtocolError("continuation after final message frame")
		}
		c.readFinal = final
	default:
		return noFrame, c.handleProtocolError("unknown opcode " + strconv.Itoa(frameType))
	}

	// 3. Read and parse frame length as per
	// https://tools.ietf.org/html/rfc6455#section-5.2
	//
	// The length of the "Payload data", in bytes: if 0-125, that is the payload
	// length.
	// - If 126, the following 2 bytes interpreted as a 16-bit unsigned
	// integer are the payload length.
	// - If 127, the following 8 bytes interpreted as
	// a 64-bit unsigned integer (the most significant bit MUST be 0) are the
	// payload length. Multibyte length quantities are expressed in network byte
	// order.
	// 设置有效负载长度
	switch c.readRemaining {
	// 如果是126，那么接下来的2个bytes解释为16bit的无符号整形作为负载数据的长度  网络序  大端存储
	case 126:
		p, err := c.read(2)
		if err != nil {
			return noFrame, err
		}

		if err := c.setReadRemaining(int64(binary.BigEndian.Uint16(p))); err != nil {
			return noFrame, err
		}
	// 如果是127，那么接下来的8个bytes解释为一个64bit的无符号整形（最高位的bit必须为0）作为负载数据的长度 网络序 大端存储
	case 127:
		p, err := c.read(8)
		if err != nil {
			return noFrame, err
		}

		if err := c.setReadRemaining(int64(binary.BigEndian.Uint64(p))); err != nil {
			return noFrame, err
		}
	}

	// 4. Handle frame masking.
	// 所有的从客户端发送到服务端的帧都需要设置这个bit位为1。
	if mask != c.isServer {
		return noFrame, c.handleProtocolError("incorrect mask flag")
	}

	// 读取Masking-key 4bytes
	if mask {
		c.readMaskPos = 0
		p, err := c.read(len(c.readMaskKey))
		if err != nil {
			return noFrame, err
		}
		copy(c.readMaskKey[:], p)
	}

	// 5. For text and binary messages, enforce read limit and return.
	//	数据帧外部读取  此处只做错误判断
	if frameType == continuationFrame || frameType == TextMessage || frameType == BinaryMessage {

		c.readLength += c.readRemaining
		// Don't allow readLength to overflow in the presence of a large readRemaining
		// counter.
		if c.readLength < 0 {
			return noFrame, ErrReadLimit
		}
		// 数据过大 无法处理  返回1009错误
		if c.readLimit > 0 && c.readLength > c.readLimit {
			c.WriteControl(CloseMessage, FormatCloseMessage(CloseMessageTooBig, ""), time.Now().Add(writeWait))
			return noFrame, ErrReadLimit
		}

		return frameType, nil
	}

	// 6. Read control frame payload.
	// 控制帧在此处理
	var payload []byte
	if c.readRemaining > 0 {
		// 控制帧数据最多125字节
		payload, err = c.read(int(c.readRemaining))
		c.setReadRemaining(0)
		if err != nil {
			return noFrame, err
		}
		if c.isServer { // 客户端发向服务端   异或回原始数据
			maskBytes(c.readMaskKey, 0, payload)
		}
	}

	// 7. Process control frame payload.

	switch frameType {
	case PongMessage:
		// 客户端ping消息   默认ponghandler直接返回nil
		if err := c.handlePong(string(payload)); err != nil {
			return noFrame, err
		}
	case PingMessage:
		// 客户端ping  默认pinghandler回复客户端payload数据
		if err := c.handlePing(string(payload)); err != nil {
			return noFrame, err
		}
	case CloseMessage:
		// 客户端关闭消息
		closeCode := CloseNoStatusReceived
		closeText := ""
		/*
			关闭帧可能包含内容（body）（帧的“应用数据”部分）来表明连接关闭的原因，例如终端的断开，
			或者是终端收到了一个太大的帧，或者是终端收到了一个不符合预期的格式的内容。如果这个内容存在，
			内容的前两个字节必须是一个无符号整型（按照网络字节序）来代表在7.4节中定义的状态码。跟在这两个
			整型字节之后的可以是UTF-8编码的的数据值（原因），数据值的定义不在此文档中。
		*/
		if len(payload) >= 2 {
			closeCode = int(binary.BigEndian.Uint16(payload))
			if !isValidReceivedCloseCode(closeCode) {
				return noFrame, c.handleProtocolError("invalid close code")
			}
			closeText = string(payload[2:])
			if !utf8.ValidString(closeText) {
				return noFrame, c.handleProtocolError("invalid utf8 payload in close frame")
			}
		}
		// 默认closehandler只发送closecode
		if err := c.handleClose(closeCode, closeText); err != nil {
			return noFrame, err
		}
		return noFrame, &CloseError{Code: closeCode, Text: closeText}
	}

	return frameType, nil
}

func (c *Conn) handleProtocolError(message string) error {
	c.WriteControl(CloseMessage, FormatCloseMessage(CloseProtocolError, message), time.Now().Add(writeWait))
	return errors.New("websocket: " + message)
}

// NextReader returns the next data message received from the peer. The
// returned messageType is either TextMessage or BinaryMessage.
//
// There can be at most one open reader on a connection. NextReader discards
// the previous message if the application has not already consumed it.
//
// Applications must break out of the application's read loop when this method
// returns a non-nil error value. Errors returned from this method are
// permanent. Once this method returns a non-nil error, all subsequent calls to
// this method return the same error.
func (c *Conn) NextReader() (messageType int, r io.Reader, err error) {
	// Close previous reader, only relevant for decompression.
	// 同一时刻只能有一个上层的reader操作底层的br
	if c.reader != nil {
		c.reader.Close()
		c.reader = nil
	}

	c.messageReader = nil
	c.readLength = 0

	for c.readErr == nil {
		frameType, err := c.advanceFrame()
		if err != nil {
			c.readErr = hideTempErr(err)
			break
		}

		if frameType == TextMessage || frameType == BinaryMessage {
			// 将底层read方法返回给上层
			c.messageReader = &messageReader{c}
			c.reader = c.messageReader
			if c.readDecompress { // rsv1  必须设置为0，除非扩展了非0值含义的扩展。
				c.reader = c.newDecompressionReader(c.reader)
			}
			return frameType, c.reader, nil
		}
	}

	// Applications that do handle the error returned from this method spin in
	// tight loop on connection failure. To help application developers detect
	// this error, panic on repeated reads to the failed connection.
	c.readErrCount++
	if c.readErrCount >= 1000 {
		panic("repeated read on failed websocket connection")
	}

	return noFrame, nil, c.readErr
}

type messageReader struct{ c *Conn }

func (r *messageReader) Read(b []byte) (int, error) {
	c := r.c
	if c.messageReader != r {
		return 0, io.EOF
	}

	for c.readErr == nil {
		if c.readRemaining > 0 {
			if int64(len(b)) > c.readRemaining {
				b = b[:c.readRemaining]
			}
			n, err := c.br.Read(b)
			c.readErr = hideTempErr(err)
			if c.isServer {
				c.readMaskPos = maskBytes(c.readMaskKey, c.readMaskPos, b[:n])
			}
			// 更新剩余字节数
			rem := c.readRemaining
			rem -= int64(n)
			c.setReadRemaining(rem)
			if c.readRemaining > 0 && c.readErr == io.EOF {
				c.readErr = errUnexpectedEOF
			}
			return n, c.readErr
		} // 当前帧的payload读完

		// 最后一帧
		if c.readFinal {
			c.messageReader = nil
			return 0, io.EOF
		}
		// readFinal为false 说明还有更多帧  继续调用advanceframe读取下一帧
		// 下一帧的帧类型只能是continueframe或者noframe(control frame 返回noframe)
		// 之后继续读取下一帧的数据，直到该片段的数据被读取完
		// 如果读取到的帧类型为noframe，则readRemainig==0，此时不做read操作，继续读取下一帧
		frameType, err := c.advanceFrame()
		switch {
		case err != nil:
			c.readErr = hideTempErr(err)
		case frameType == TextMessage || frameType == BinaryMessage:
			c.readErr = errors.New("websocket: internal error, unexpected text or binary in Reader")
		}
	}

	err := c.readErr
	if err == io.EOF && c.messageReader == r {
		err = errUnexpectedEOF
	}
	return 0, err
}

func (r *messageReader) Close() error {
	return nil
}

// ReadMessage is a helper method for getting a reader using NextReader and
// reading from that reader to a buffer.
func (c *Conn) ReadMessage() (messageType int, p []byte, err error) {
	var r io.Reader
	messageType, r, err = c.NextReader()
	if err != nil {
		return messageType, nil, err
	}
	// ReadAll 调用 messageReader的read方法
	p, err = ioutil.ReadAll(r)
	return messageType, p, err
}

// SetReadDeadline sets the read deadline on the underlying network connection.
// After a read has timed out, the websocket connection state is corrupt and
// all future reads will return an error. A zero value for t means reads will
// not time out.
func (c *Conn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

// SetReadLimit sets the maximum size in bytes for a message read from the peer. If a
// message exceeds the limit, the connection sends a close message to the peer
// and returns ErrReadLimit to the application.
func (c *Conn) SetReadLimit(limit int64) {
	c.readLimit = limit
}

// CloseHandler returns the current close handler
func (c *Conn) CloseHandler() func(code int, text string) error {
	return c.handleClose
}

// SetCloseHandler sets the handler for close messages received from the peer.
// The code argument to h is the received close code or CloseNoStatusReceived
// if the close message is empty. The default close handler sends a close
// message back to the peer.
//
// The handler function is called from the NextReader, ReadMessage and message
// reader Read methods. The application must read the connection to process
// close messages as described in the section on Control Messages above.
//
// The connection read methods return a CloseError when a close message is
// received. Most applications should handle close messages as part of their
// normal error handling. Applications should only set a close handler when the
// application must perform some action before sending a close message back to
// the peer.
func (c *Conn) SetCloseHandler(h func(code int, text string) error) {
	if h == nil {
		h = func(code int, text string) error {
			message := FormatCloseMessage(code, "")
			c.WriteControl(CloseMessage, message, time.Now().Add(writeWait))
			return nil
		}
	}
	c.handleClose = h
}

// PingHandler returns the current ping handler
func (c *Conn) PingHandler() func(appData string) error {
	return c.handlePing
}

// SetPingHandler sets the handler for ping messages received from the peer.
// The appData argument to h is the PING message application data. The default
// ping handler sends a pong to the peer.
//
// The handler function is called from the NextReader, ReadMessage and message
// reader Read methods. The application must read the connection to process
// ping messages as described in the section on Control Messages above.
func (c *Conn) SetPingHandler(h func(appData string) error) {
	if h == nil {
		h = func(message string) error {
			err := c.WriteControl(PongMessage, []byte(message), time.Now().Add(writeWait))
			if err == ErrCloseSent {
				return nil
			} else if e, ok := err.(net.Error); ok && e.Temporary() {
				return nil
			}
			return err
		}
	}
	c.handlePing = h
}

// PongHandler returns the current pong handler
func (c *Conn) PongHandler() func(appData string) error {
	return c.handlePong
}

// SetPongHandler sets the handler for pong messages received from the peer.
// The appData argument to h is the PONG message application data. The default
// pong handler does nothing.
//
// The handler function is called from the NextReader, ReadMessage and message
// reader Read methods. The application must read the connection to process
// pong messages as described in the section on Control Messages above.
func (c *Conn) SetPongHandler(h func(appData string) error) {
	if h == nil {
		h = func(string) error { return nil }
	}
	c.handlePong = h
}

// UnderlyingConn returns the internal net.Conn. This can be used to further
// modifications to connection specific flags.
func (c *Conn) UnderlyingConn() net.Conn {
	return c.conn
}

// EnableWriteCompression enables and disables write compression of
// subsequent text and binary messages. This function is a noop if
// compression was not negotiated with the peer.
func (c *Conn) EnableWriteCompression(enable bool) {
	c.enableWriteCompression = enable
}

// SetCompressionLevel sets the flate compression level for subsequent text and
// binary messages. This function is a noop if compression was not negotiated
// with the peer. See the compress/flate package for a description of
// compression levels.
func (c *Conn) SetCompressionLevel(level int) error {
	if !isValidCompressionLevel(level) {
		return errors.New("websocket: invalid compression level")
	}
	c.compressionLevel = level
	return nil
}

// FormatCloseMessage formats closeCode and text as a WebSocket close message.
// An empty message is returned for code CloseNoStatusReceived.
func FormatCloseMessage(closeCode int, text string) []byte {
	if closeCode == CloseNoStatusReceived {
		// Return empty message because it's illegal to send
		// CloseNoStatusReceived. Return non-nil value in case application
		// checks for nil.
		return []byte{}
	}
	buf := make([]byte, 2+len(text))
	binary.BigEndian.PutUint16(buf, uint16(closeCode))
	copy(buf[2:], text)
	return buf
}
