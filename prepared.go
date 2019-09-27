// Copyright 2017 The Gorilla WebSocket Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package websocket

import (
	"bytes"
	"net"
	"sync"
	"time"
)

// PreparedMessage caches on the wire representations of a message payload.
// Use PreparedMessage to efficiently send a message payload to multiple
// connections. PreparedMessage is especially useful when compression is used
// because the CPU and memory expensive compression operation can be executed
// once for a given set of compression options.
// 在有效负载的线路上缓存一个消息，而后可以有效的将该消息发往多个连接。当使用数据压缩是非常有效
// CPU和内存只需要操作一次
type PreparedMessage struct {
	messageType int    // 消息类型
	data        []byte // 消息payload
	mu          sync.Mutex
	frames      map[prepareKey]*preparedFrame
}

// prepareKey defines a unique set of options to cache prepared frames in PreparedMessage.
type prepareKey struct {
	isServer         bool
	compress         bool
	compressionLevel int
}

// preparedFrame contains data in wire representation.
type preparedFrame struct {
	once sync.Once
	data []byte
}

// NewPreparedMessage returns an initialized PreparedMessage. You can then send
// it to connection using WritePreparedMessage method. Valid wire
// representation will be calculated lazily only once for a set of current
// connection options.
func NewPreparedMessage(messageType int, data []byte) (*PreparedMessage, error) {
	pm := &PreparedMessage{
		messageType: messageType,
		frames:      make(map[prepareKey]*preparedFrame),
		data:        data,
	}

	// Prepare a plain server frame.
	_, frameData, err := pm.frame(prepareKey{isServer: true, compress: false})
	if err != nil {
		return nil, err
	}

	// To protect against caller modifying the data argument, remember the data
	// copied to the plain server frame.
	pm.data = frameData[len(frameData)-len(data):]
	return pm, nil
}

// 第一次调用时会生成frame.data 后续调用将直接返回frame.data
func (pm *PreparedMessage) frame(key prepareKey) (int, []byte, error) {
	pm.mu.Lock()
	frame, ok := pm.frames[key]
	if !ok {
		frame = &preparedFrame{}
		pm.frames[key] = frame
	}
	pm.mu.Unlock()

	var err error
	frame.once.Do(func() {
		// Prepare a frame using a 'fake' connection.
		// TODO: Refactor code in conn.go to allow more direct construction of
		// the frame.
		// 使用一个伪连接  这样可以使用WriteMessage方法将数据组装成帧，然后写到prepareConn的buffer中
		mu := make(chan bool, 1)
		mu <- true
		var nc prepareConn
		c := &Conn{
			conn:                   &nc,
			mu:                     mu,
			isServer:               key.isServer,
			compressionLevel:       key.compressionLevel,
			enableWriteCompression: true,
			writeBuf:               make([]byte, defaultWriteBufferSize+maxFrameHeaderSize),
		}
		if key.compress {
			c.newCompressionWriter = compressNoContextTakeover
		}
		// 格式化成帧的数据保存在c.nc.buf中
		err = c.WriteMessage(pm.messageType, pm.data)
		// 将帧数据赋值给frame.data
		frame.data = nc.buf.Bytes()
	})
	return pm.messageType, frame.data, err
}

type prepareConn struct {
	buf bytes.Buffer
	net.Conn
}

func (pc *prepareConn) Write(p []byte) (int, error)        { return pc.buf.Write(p) }
func (pc *prepareConn) SetWriteDeadline(t time.Time) error { return nil }
