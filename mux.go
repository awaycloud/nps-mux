package nps_mux

import (
	"errors"
	"io"
	"math"
	"net"
	"sync/atomic"
	"time"

	"github.com/astaxie/beego/logs"
)

const (
	muxPingFlag uint8 = iota
	muxNewConnOk
	muxNewConnFail
	muxNewMsg
	muxNewMsgPart
	muxMsgSendOk
	muxNewConn
	muxConnClose
	muxPingReturn
	muxPing            int32 = -1
	maximumSegmentSize       = poolSizeWindow
	maximumWindowSize        = 1 << 25 // 1<<31-1 TCP slide window size is very large,
	// we use 32M, reduce memory usage
)

type Mux struct {
	latency uint64 // we store latency in bits, but it's float64
	net.Listener
	conn          net.Conn
	connMap       *connMap
	newConnCh     chan *conn
	id            int32
	closeChan     chan struct{}
	IsClose       bool
	pingOk        uint32
	counter       *latencyCounter
	bw            *bandwidth
	pingCh        chan []byte
	pingCheckTime uint32
	connType      string
	writeQueue    priorityQueue
	newConnQueue  connQueue
}

func NewMux(c net.Conn, connType string) *Mux {
	//c.(*net.TCPConn).SetReadBuffer(0)
	//c.(*net.TCPConn).SetWriteBuffer(0)
	m := &Mux{
		conn:      c,
		connMap:   NewConnMap(),
		id:        0,
		closeChan: make(chan struct{}, 1),
		newConnCh: make(chan *conn),
		bw:        new(bandwidth),
		IsClose:   false,
		connType:  connType,
		pingCh:    make(chan []byte),
		counter:   newLatencyCounter(),
	}
	m.writeQueue.New()
	m.newConnQueue.New()
	//read session by flag
	m.readSession()
	//ping
	m.ping()
	m.writeSession()
	return m
}

func (s *Mux) NewConn() (*conn, error) {
	if s.IsClose {
		return nil, errors.New("the mux has closed")
	}
	conn := NewConn(s.getId(), s)
	//it must be Set before send
	s.connMap.Set(conn.connId, conn)
	s.sendInfo(muxNewConn, conn.connId, nil)
	//Set a timer timeout 120 second
	timer := time.NewTimer(time.Minute * 2)
	defer timer.Stop()
	select {
	case <-conn.connStatusOkCh:
		return conn, nil
	case <-timer.C:
	}
	return nil, errors.New("create connection fail，the server refused the connection")
}

func (s *Mux) Accept() (net.Conn, error) {
	if s.IsClose {
		return nil, errors.New("accpet error,the mux has closed")
	}
	conn := <-s.newConnCh
	if conn == nil {
		return nil, errors.New("accpet error,the conn has closed")
	}
	return conn, nil
}

func (s *Mux) Addr() net.Addr {
	return s.conn.LocalAddr()
}

func (s *Mux) sendInfo(flag uint8, id int32, data interface{}) {
	if s.IsClose {
		return
	}
	var err error
	pack := muxPack.Get()
	err = pack.Set(flag, id, data)
	if err != nil {
		muxPack.Put(pack)
		logs.Error("mux: New Pack err", err)
		_ = s.Close()
		return
	}
	s.writeQueue.Push(pack)
	return
}

func (s *Mux) writeSession() {
	go func() {
		for {
			if s.IsClose {
				break
			}
			pack := s.writeQueue.Pop()
			if s.IsClose {
				break
			}
			err := pack.Pack(s.conn)
			muxPack.Put(pack)
			if err != nil {
				logs.Error("mux: Pack err", err)
				_ = s.Close()
				break
			}
		}
	}()
}

func (s *Mux) ping() {
	go func() {
		now, _ := time.Now().UTC().MarshalText()
		s.sendInfo(muxPingFlag, muxPing, now)
		// send the ping flag and Get the latency first
		ticker := time.NewTicker(time.Second * 5)
		defer ticker.Stop()
		for {
			if s.IsClose {
				break
			}
			select {
			case <-ticker.C:
			}
			if atomic.LoadUint32(&s.pingCheckTime) >= 60 {
				logs.Error("mux: ping time out")
				_ = s.Close()
				// more than 5 minutes not receive the ping return package,
				// mux conn is damaged, maybe a packet drop, close it
				break
			}
			now, _ := time.Now().UTC().MarshalText()
			s.sendInfo(muxPingFlag, muxPing, now)
			atomic.AddUint32(&s.pingCheckTime, 1)
			if atomic.LoadUint32(&s.pingOk) > 10 && s.connType == "kcp" {
				logs.Error("mux: kcp ping err")
				_ = s.Close()
				break
			}
			atomic.AddUint32(&s.pingOk, 1)
		}
		return
	}()

	go func() {
		var now time.Time
		var data []byte
		for {
			if s.IsClose {
				break
			}
			select {
			case data = <-s.pingCh:
				atomic.StoreUint32(&s.pingCheckTime, 0)
			case <-s.closeChan:
				break
			}
			_ = now.UnmarshalText(data)
			latency := time.Now().UTC().Sub(now).Seconds() / 2
			if latency > 0 {
				atomic.StoreUint64(&s.latency, math.Float64bits(s.counter.Latency(latency)))
				// convert float64 to bits, store it atomic
			}
			if cap(data) > 0 {
				windowBuff.Put(data)
			}
		}
	}()
}

func (s *Mux) readSession() {
	go func() {
		var connection *conn
		for {
			if s.IsClose {
				break
			}
			connection = s.newConnQueue.Pop()
			if s.IsClose {
				break // make sure that is closed
			}
			s.connMap.Set(connection.connId, connection) //it has been Set before send ok
			s.newConnCh <- connection
			s.sendInfo(muxNewConnOk, connection.connId, nil)
		}
	}()
	go func() {
		pack := muxPack.Get()
		var l uint16
		var err error
		for {
			if s.IsClose {
				break
			}
			pack = muxPack.Get()
			s.bw.StartRead()
			if l, err = pack.UnPack(s.conn); err != nil {
				logs.Error("mux: read session unpack from connection err", err)
				_ = s.Close()
				break
			}
			s.bw.SetCopySize(l)
			atomic.StoreUint32(&s.pingOk, 0)
			switch pack.flag {
			case muxNewConn: //New connection
				connection := NewConn(pack.id, s)
				s.newConnQueue.Push(connection)
				continue
			case muxPingFlag: //ping
				s.sendInfo(muxPingReturn, muxPing, pack.content)
				windowBuff.Put(pack.content)
				continue
			case muxPingReturn:
				s.pingCh <- pack.content
				continue
			}
			if connection, ok := s.connMap.Get(pack.id); ok && !connection.isClose {
				switch pack.flag {
				case muxNewMsg, muxNewMsgPart: //New msg from remote connection
					err = s.newMsg(connection, pack)
					if err != nil {
						logs.Error("mux: read session connection New msg err", err)
						_ = connection.Close()
					}
					continue
				case muxNewConnOk: //connection ok
					connection.connStatusOkCh <- struct{}{}
					continue
				case muxNewConnFail:
					connection.connStatusFailCh <- struct{}{}
					continue
				case muxMsgSendOk:
					if connection.isClose {
						continue
					}
					connection.sendWindow.SetSize(pack.remainLength)
					continue
				case muxConnClose: //close the connection
					connection.closingFlag = true
					connection.receiveWindow.Stop() // close signal to receive window
					continue
				}
			} else if pack.flag == muxConnClose {
				continue
			}
			muxPack.Put(pack)
		}
		muxPack.Put(pack)
		_ = s.Close()
	}()
}

func (s *Mux) newMsg(connection *conn, pack *muxPackager) (err error) {
	if connection.isClose {
		err = io.ErrClosedPipe
		return
	}
	//insert into queue
	if pack.flag == muxNewMsgPart {
		err = connection.receiveWindow.Write(pack.content, pack.length, true, pack.id)
	}
	if pack.flag == muxNewMsg {
		err = connection.receiveWindow.Write(pack.content, pack.length, false, pack.id)
	}
	return
}

func (s *Mux) Close() (err error) {
	logs.Warn("close mux")
	if s.IsClose {
		return errors.New("the mux has closed")
	}
	s.IsClose = true
	s.connMap.Close()
	s.connMap = nil
	s.closeChan <- struct{}{}
	close(s.newConnCh)
	err = s.conn.Close()
	s.release()
	return
}

func (s *Mux) release() {
	for {
		pack := s.writeQueue.TryPop()
		if pack == nil {
			break
		}
		if pack.basePackager.content != nil {
			windowBuff.Put(pack.basePackager.content)
		}
		muxPack.Put(pack)
	}
	for {
		connection := s.newConnQueue.TryPop()
		if connection == nil {
			break
		}
		connection = nil
	}
	s.writeQueue.Stop()
	s.newConnQueue.Stop()
}

//Get New connId as unique flag
func (s *Mux) getId() (id int32) {
	//Avoid going beyond the scope
	if (math.MaxInt32 - s.id) < 10000 {
		atomic.StoreInt32(&s.id, 0)
	}
	id = atomic.AddInt32(&s.id, 1)
	if _, ok := s.connMap.Get(id); ok {
		return s.getId()
	}
	return
}

type bandwidth struct {
	readBandwidth uint64 // store in bits, but it's float64
	readStart     time.Time
	lastReadStart time.Time
	bufLength     uint32
}

func (Self *bandwidth) StartRead() {
	if Self.readStart.IsZero() {
		Self.readStart = time.Now()
	}
	if Self.bufLength >= maximumSegmentSize*300 {
		Self.lastReadStart, Self.readStart = Self.readStart, time.Now()
		Self.calcBandWidth()
	}
}

func (Self *bandwidth) SetCopySize(n uint16) {
	Self.bufLength += uint32(n)
}

func (Self *bandwidth) calcBandWidth() {
	t := Self.readStart.Sub(Self.lastReadStart)
	atomic.StoreUint64(&Self.readBandwidth, math.Float64bits(float64(Self.bufLength)/t.Seconds()))
	Self.bufLength = 0
}

func (Self *bandwidth) Get() (bw float64) {
	// The zero value, 0 for numeric types
	bw = math.Float64frombits(atomic.LoadUint64(&Self.readBandwidth))
	if bw <= 0 {
		bw = 100
	}
	return
}

const counterBits = 4
const counterMask = 1<<counterBits - 1

func newLatencyCounter() *latencyCounter {
	return &latencyCounter{
		buf:     make([]float64, 1<<counterBits, 1<<counterBits),
		headMin: 0,
	}
}

type latencyCounter struct {
	buf []float64 //buf is a fixed length ring buffer,
	// if buffer is full, New value will replace the oldest one.
	headMin uint8 //head indicate the head in ring buffer,
	// in meaning, slot in list will be replaced;
	// min indicate this slot value is minimal in list.
}

func (Self *latencyCounter) unpack(idxs uint8) (head, min uint8) {
	head = (idxs >> counterBits) & counterMask
	// we Set head is 4 bits
	min = idxs & counterMask
	return
}

func (Self *latencyCounter) pack(head, min uint8) uint8 {
	return head<<counterBits |
		min&counterMask
}

func (Self *latencyCounter) add(value float64) {
	head, min := Self.unpack(Self.headMin)
	Self.buf[head] = value
	if head == min {
		min = Self.minimal()
		//if head equals min, means the min slot already be replaced,
		// so we need to find another minimal value in the list,
		// and change the min indicator
	}
	if Self.buf[min] > value {
		min = head
	}
	head++
	Self.headMin = Self.pack(head, min)
}

func (Self *latencyCounter) minimal() (min uint8) {
	var val float64
	var i uint8
	for i = 0; i < counterMask; i++ {
		if Self.buf[i] > 0 {
			if val > Self.buf[i] {
				val = Self.buf[i]
				min = i
			}
		}
	}
	return
}

func (Self *latencyCounter) Latency(value float64) (latency float64) {
	Self.add(value)
	_, min := Self.unpack(Self.headMin)
	latency = Self.buf[min] * Self.countSuccess()
	return
}

const lossRatio = 1.6

func (Self *latencyCounter) countSuccess() (successRate float64) {
	var success, loss, i uint8
	_, min := Self.unpack(Self.headMin)
	for i = 0; i < counterMask; i++ {
		if Self.buf[i] > lossRatio*Self.buf[min] && Self.buf[i] > 0 {
			loss++
		}
		if Self.buf[i] <= lossRatio*Self.buf[min] && Self.buf[i] > 0 {
			success++
		}
	}
	// counting all the data in the ring buf, except zero
	successRate = float64(success) / float64(loss+success)
	return
}