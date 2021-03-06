package connection

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/Dongxiem/fastnet/eventloop"
	"github.com/Dongxiem/fastnet/log"
	"github.com/Dongxiem/fastnet/poller"
	"github.com/Dongxiem/fastnet/tool/ringbuffer"
	"github.com/Dongxiem/fastnet/tool/ringbuffer/pool"
	"github.com/Dongxiem/fastnet/tool/sync/atomic"
	"github.com/RussellLuo/timingwheel"
	"github.com/gobwas/pool/pbytes"
	"golang.org/x/sys/unix"
)

// CallBack : 回调接口
type CallBack interface {
	OnMessage(c *Connection, ctx interface{}, data []byte) []byte
	OnClose(c *Connection)
}

// Connection：TCP 连接结构体
type Connection struct {
	fd        int
	connected atomic.Bool
	outBuffer *ringbuffer.RingBuffer 	// 写 buffer
	inBuffer  *ringbuffer.RingBuffer 	// 读 buffer
	callBack  CallBack					// 回调方法
	loop      *eventloop.EventLoop		// 循环调度
	peerAddr  string
	ctx       interface{}
	KeyValueContext

	idleTime    time.Duration
	activeTime  atomic.Int64
	timingWheel *timingwheel.TimingWheel

	protocol Protocol					// 使用协议
}

// ErrConnectionClosed：生成新错误连接已关闭
var ErrConnectionClosed = errors.New("connection closed")

// New：创建 Connection
func New(fd int, loop *eventloop.EventLoop, sa unix.Sockaddr, protocol Protocol, tw *timingwheel.TimingWheel, idleTime time.Duration, callBack CallBack) *Connection {
	conn := &Connection{
		fd:          fd,
		peerAddr:    sockAddrToString(sa),
		outBuffer:   pool.Get(),
		inBuffer:    pool.Get(),
		callBack:    callBack,
		loop:        loop,
		idleTime:    idleTime,
		timingWheel: tw,
		protocol:    protocol,
	}
	conn.connected.Set(true)

	if conn.idleTime > 0 {
		_ = conn.activeTime.Swap(time.Now().Unix())
		conn.timingWheel.AfterFunc(conn.idleTime, conn.closeTimeoutConn())
	}

	return conn
}

// closeTimeoutConn：关闭超时的连接
func (c *Connection) closeTimeoutConn() func() {
	return func() {
		now := time.Now()
		intervals := now.Sub(time.Unix(c.activeTime.Get(), 0))
		// 判断时间差
		if intervals >= c.idleTime {
			_ = c.Close()
		} else {
			c.timingWheel.AfterFunc(c.idleTime-intervals, c.closeTimeoutConn())
		}
	}
}

// Context：获取 Context
func (c *Connection) Context() interface{} {
	return c.ctx
}

// SetContext：设置 Context
func (c *Connection) SetContext(ctx interface{}) {
	c.ctx = ctx
}

// PeerAddr：获取客户端地址信息
func (c *Connection) PeerAddr() string {
	return c.peerAddr
}

// Connected：测试是否已连接
func (c *Connection) Connected() bool {
	return c.connected.Get()
}

// Send：进行发送数据
func (c *Connection) Send(buffer []byte) error {
	// 如果未连接或连接已断开
	if !c.connected.Get() {
		return ErrConnectionClosed
	}

	// 循环调用 sendInLoop 方法
	c.loop.QueueInLoop(func() {
		// 进行协议打包封装之后再发送
		c.sendInLoop(c.protocol.Packet(c, buffer))
	})
	return nil
}

// Close：关闭连接
func (c *Connection) Close() error {
	// 如果不能获取当前连接，则报错
	if !c.connected.Get() {
		return ErrConnectionClosed
	}
	// 进去循环 loop中调用关闭函数
	c.loop.QueueInLoop(func() {
		c.handleClose(c.fd)
	})
	return nil
}

// ShutdownWrite：关闭可写端，等待读取完接收缓冲区所有数据
func (c *Connection) ShutdownWrite() error {
	c.connected.Set(false)
	return unix.Shutdown(c.fd, unix.SHUT_WR)
}

// HandleEvent：内部使用，event loop 回调
func (c *Connection) HandleEvent(fd int, events poller.Event) {
	if c.idleTime > 0 {
		_ = c.activeTime.Swap(time.Now().Unix())
	}

	if events&poller.EventErr != 0 {
		c.handleClose(fd)
		return
	}

	// 判断 outBuffer 是否为空
	if c.outBuffer.Length() != 0 {
		// 再接着判断 events 是为读事件还是写事件
		if events&poller.EventWrite != 0 {
			// 处理写事件
			c.handleWrite(fd)
		} else if events&poller.EventRead != 0 {
			// 处理读事件
			c.handleRead(fd)
		}
	}
}

// handlerProtocol：处理协议相关内容
func (c *Connection) handlerProtocol(buffer *ringbuffer.RingBuffer) []byte {
	// 在调用方函数里归还
	out := pbytes.GetCap(1024)
	ctx, receivedData := c.protocol.UnPacket(c, buffer)
	for ctx != nil || len(receivedData) != 0 {
		// 调用 OnMessage 进行相对应的处理后得到 sendData
		sendData := c.callBack.OnMessage(c, ctx, receivedData)
		// 如果 sendData 长度大于 0，则插入 out 当中
		if len(sendData) > 0 {
			out = append(out, c.protocol.Packet(c, sendData)...)
		}

		ctx, receivedData = c.protocol.UnPacket(c, buffer)
	}
	return out
}

// handleRead：处理读事件
func (c *Connection) handleRead(fd int) {
	// TODO 避免这次内存拷贝
	// 获得当前 buf，并通过读系统调用写入到 buf
	buf := c.loop.PacketBuf()
	n, err := unix.Read(c.fd, buf)
	// 错误处理，非阻塞IO 缓冲区未准备数据可供读则返回错误为 EAGAIN
	if n == 0 || err != nil {
		if err != unix.EAGAIN {
			c.handleClose(fd)
		}
		return
	}

	if c.inBuffer.Length() == 0 {
		// 1. 如果 inBuffer 为空
		// 通过 ringbuffer.NewWithData 传入 buf[:n] 创建一个新的 buffer
		buffer := ringbuffer.NewWithData(buf[:n])
		// 使用 handlerProtocol 进行解析得到 out
		out := c.handlerProtocol(buffer)
		// 如果此时 buffer 长度不为 0，则获取其内容并写入到 inBuffer 中
		if buffer.Length() > 0 {
			first, _ := buffer.PeekAll()
			_, _ = c.inBuffer.Write(first)
		}
		if len(out) != 0 {
			// 处理 out 写buffer
			c.sendInLoop(out)
		}

		pbytes.Put(out)
	} else {
		// 2. 如果 inBuffer 不为空，则写入到 inBuffer 中
		_, _ = c.inBuffer.Write(buf[:n])
		out := c.handlerProtocol(c.inBuffer)
		if len(out) != 0 {
			c.sendInLoop(out)
		}

		pbytes.Put(out)
	}
}

// handleWrite：处理写事件
func (c *Connection) handleWrite(fd int) {
	// 从 outBuffer 取出数据
	first, end := c.outBuffer.PeekAll()
	n, err := unix.Write(c.fd, first)
	// 错误处理，非阻塞IO 缓冲区没有空间可供写则返回错误为 EAGAIN
	if err != nil {
		// 返回 EAGAIN 并不做其他动作，将数据保存在 outBuffer 中，等待下次写
		if err == unix.EAGAIN {
			return
		}
		c.handleClose(fd)
		return
	}
	// 清楚部分数据
	c.outBuffer.Retrieve(n)

	// 再进行判断 end 是否有数据，有则同样处理
	if n == len(first) && len(end) > 0 {
		n, err = unix.Write(c.fd, end)
		// 错误处理，非阻塞IO 缓冲区没有空间可供写则返回错误为 EAGAIN
		if err != nil {
			if err == unix.EAGAIN {
				return
			}
			c.handleClose(fd)
			return
		}
		c.outBuffer.Retrieve(n)
	}

	// 处理完了之后，通知 fd 可读
	if c.outBuffer.Length() == 0 {
		if err := c.loop.EnableRead(fd); err != nil {
			log.Error("[EnableRead]", err)
		}
	}
}

// handleClose：处理关闭事件
func (c *Connection) handleClose(fd int) {
	if c.connected.Get() {
		c.connected.Set(false)
		c.loop.DeleteFdInLoop(fd)

		// 关闭事件会调用 OnClose
		c.callBack.OnClose(c)
		if err := unix.Close(fd); err != nil {
			log.Error("[close fd]", err)
		}

		pool.Put(c.inBuffer)
		pool.Put(c.outBuffer)
	}
}

// sendInLoop：送入循环，data 为经过协议处理过后的数据
func (c *Connection) sendInLoop(data []byte) {
	if c.outBuffer.Length() > 0 {
		// 如果 outBuffer 长度不为 0，则直接将 outBuffer 写入到 outBuffer
		_, _ = c.outBuffer.Write(data)
	} else {
		// 否则直接调用写系统调用，将数据写入到 fd 对应的的文件中
		n, err := unix.Write(c.fd, data)
		// 错误处理，非阻塞IO 缓冲区无位置可供写则返回错误为 EAGAIN
		if err != nil {
			if err == unix.EAGAIN {
				return
			}
			c.handleClose(c.fd)
			return
		}
		if n == 0 {
			// 如果写入的大小为 0，则将所有的 data 写入到 outBuffer 中
			_, _ = c.outBuffer.Write(data)
		} else if n < len(data) {
			// 如果写入的大小为 n 且不为0，则将其剩余部分内容保存起来
			_, _ = c.outBuffer.Write(data[n:])
		}

		// 通知可读可写
		if c.outBuffer.Length() > 0 {
			_ = c.loop.EnableReadWrite(c.fd)
		}
	}
}

// sockAddrToString：将 socket 转为字符串格式
func sockAddrToString(sa unix.Sockaddr) string {
	switch sa := (sa).(type) {
	case *unix.SockaddrInet4:
		return net.JoinHostPort(net.IP(sa.Addr[:]).String(), strconv.Itoa(sa.Port))
	case *unix.SockaddrInet6:
		return net.JoinHostPort(net.IP(sa.Addr[:]).String(), strconv.Itoa(sa.Port))
	default:
		return fmt.Sprintf("(unknown - %T)", sa)
	}
}
