package session

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	log "github.com/blackbeans/log4go"
	"github.com/blackbeans/turbo"
	"github.com/blackbeans/turbo/packet"
	"io"
	"net"
	"syscall"
	"time"
)

type Session struct {
	conn         *net.TCPConn //tcp的session
	remoteAddr   string
	br           *bufio.Reader
	bw           *bufio.Writer
	idleTimer    *time.Timer        //空闲timer
	ReadChannel  chan packet.Packet //request的channel
	WriteChannel chan packet.Packet //response的channel
	isClose      bool
	rc           *turbo.RemotingConfig
}

func NewSession(conn *net.TCPConn, rc *turbo.RemotingConfig) *Session {

	conn.SetKeepAlive(true)
	conn.SetKeepAlivePeriod(3 * time.Second)
	//禁用nagle
	conn.SetNoDelay(true)
	conn.SetReadBuffer(rc.ReadBufferSize)
	conn.SetWriteBuffer(rc.WriteBufferSize)

	session := &Session{
		conn:         conn,
		br:           bufio.NewReaderSize(conn, rc.ReadBufferSize),
		bw:           bufio.NewWriterSize(conn, rc.WriteBufferSize),
		ReadChannel:  make(chan packet.Packet, rc.ReadChannelSize),
		WriteChannel: make(chan packet.Packet, rc.WriteChannelSize),
		isClose:      false,
		remoteAddr:   conn.RemoteAddr().String(),
		rc:           rc,
		idleTimer:    time.NewTimer(rc.IdleTime)}
	return session
}

func (self *Session) RemotingAddr() string {
	return self.remoteAddr
}

func (self *Session) Idle() bool {
	select {
	case <-self.idleTimer.C:
		//超时，暂时IO处于空闲
		return true
	default:
		//没有超时则是一直有读写
		return false
	}
}

//读取
func (self *Session) ReadPacket() {

	defer func() {
		if err := recover(); nil != err {
			log.Error("Session|ReadPacket|%s|recover|FAIL|%s\n", self.remoteAddr, err)
		}
	}()

	//缓存本次包的数据
	packetBuff := make([]byte, 0, self.rc.ReadBufferSize)
	buff := bytes.NewBuffer(packetBuff)

	for !self.isClose {
		slice, err := self.br.ReadSlice(packet.CMD_CRLF[0])
		//如果没有达到请求头的最小长度则继续读取
		if nil != err {
			buff.Reset()
			//链接是关闭的
			if err == io.EOF ||
				err == syscall.EPIPE ||
				err == syscall.ECONNRESET {
				self.Close()
				log.Error("Session|ReadPacket|%s|\\r|FAIL|CLOSE SESSION|%s\n", self.remoteAddr, err)
			}
			continue
		}

		//读取下一个字节
		delim, err := self.br.ReadByte()
		if nil != err {
			buff.Reset()
			//链接是关闭的
			if err == io.EOF ||
				err == syscall.EPIPE ||
				err == syscall.ECONNRESET {
				self.Close()
				log.Error("Session|ReadPacket|%s|\\r|FAIL|CLOSE SESSION|%s\n", self.remoteAddr, err)
			}
			continue
		}

		l := buff.Len() + len(slice) + 1
		//如果是\n那么就是一个完整的包
		if l >= packet.MAX_PACKET_BYTES {
			log.Error("Session|ReadPacket|%s|WRITE|TOO LARGE|CLOSE SESSION|%s\n", self.remoteAddr, err)
			self.Close()
			return
		} else if l > packet.PACKET_HEAD_LEN && delim == packet.CMD_CRLF[1] {

			b := append(buff.Bytes(), append(slice, delim)...)
			packet, err := packet.UnmarshalTLV(b)
			if nil != err || nil == packet {
				log.Error("Session|ReadPacket|UnmarshalTLV|FAIL|%s|%d|%s\n", err, buff.Len(), buff.Bytes())
				buff.Reset()
				continue
			}

			//写入缓冲
			self.ReadChannel <- *packet
			//重置buffer
			buff.Reset()
			self.idleTimer.Reset(self.rc.IdleTime)
			if nil != self.rc.FlowStat {
				self.rc.FlowStat.ReadFlow.Incr(1)
			}

		} else {
			ap := append(slice, delim)
			_, err := buff.Write(ap)
			//写入，如果数据太大直接有ErrTooLarge则关闭session退出
			if nil != err {
				self.Close()
				log.Error("Session|ReadPacket|%s|WRITE|TOO LARGE|CLOSE SESSION|%s\n", self.remoteAddr, err)
				return
			}
		}
	}
}

//写出数据
func (self *Session) Write(p packet.Packet) error {
	defer func() {
		if err := recover(); nil != err {
			log.Error("Session|Write|%s|recover|FAIL|%s\n", self.remoteAddr, err)
		}
	}()

	if !self.isClose {
		select {
		case self.WriteChannel <- p:
			return nil
		default:
			return errors.New(fmt.Sprintf("WRITE CHANNLE [%s] FULL", self.remoteAddr))
		}
	}
	return errors.New(fmt.Sprintf("Session|[%s]|CLOSED", self.remoteAddr))
}

//真正写入网络的流
func (self *Session) write0(tlv packet.Packet) {

	p := packet.MarshalPacket(&tlv)
	if nil == p || len(p) <= 0 {
		log.Error("Session|write0|MarshalPacket|FAIL|EMPTY PACKET|%s\n", tlv)
		//如果是同步写出
		return
	}

	length, err := self.bw.Write(p)
	if nil != err {
		log.Error("Session|write0|conn|%s|FAIL|%s|%d/%d\n", self.remoteAddr, err, length, len(p))
		//链接是关闭的
		if err == io.EOF ||
			err == syscall.EPIPE || err == syscall.ECONNRESET {
			self.Close()
			return
		}

		//如果没有写够则再写一次
		if err == io.ErrShortWrite {
			self.bw.Write(p[length:])
		}
	}

	if nil != self.rc.FlowStat {
		self.rc.FlowStat.WriteFlow.Incr(1)
	}

}

//写入响应
func (self *Session) WritePacket() {

	var p packet.Packet
	for !self.isClose {
		p = <-self.WriteChannel
		self.write0(p)
		self.flush()
	}
}

func (self *Session) flush() {
	if self.bw.Buffered() > 0 && !self.Closed() {
		err := self.bw.Flush()
		if nil != err {
			log.Error("Session|Write|FLUSH|FAIL|%t\n", err.Error())
			self.bw.Reset(self.conn)
		}
	}
}

//当前连接是否关闭
func (self *Session) Closed() bool {
	return self.isClose
}

func (self *Session) Close() error {

	if !self.isClose {
		self.isClose = true
		self.conn.Close()
		close(self.WriteChannel)
		close(self.ReadChannel)
		log.Debug("Session|Close|%s...\n", self.remoteAddr)
	}
	return nil
}
