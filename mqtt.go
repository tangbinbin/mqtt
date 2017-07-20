package mqtt

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
	"unsafe"
)

const wordSize = int(unsafe.Sizeof(uintptr(0)))

var bytePool *sync.Pool = &sync.Pool{New: func() interface{} { return new(bytes.Buffer) }}
var readPool *sync.Pool = &sync.Pool{New: func() interface{} { return bufio.NewReader(nil) }}

func getBufioReader(r io.Reader) *bufio.Reader {
	if v := readPool.Get(); v != nil {
		br := v.(*bufio.Reader)
		br.Reset(r)
		return br
	}
	return bufio.NewReader(r)
}

func putBufioReader(r *bufio.Reader) {
	if r != nil {
		r.Reset(nil)
		readPool.Put(r)
	}
}

type CP interface {
	Encode() []byte
	String() string
	Decode(*bufio.Reader) error
	DecodeBytes([]byte) error
}

const (
	CONNECT = byte(iota + 1)
	CONNACK
	PUBLISH
	PUBACK
	PUBREC
	PUBREL
	PUBCOMP
	SUBSCRIBE
	SUBACK
	UNSUBSCRIBE
	UNSUBACK
	PINGREQ
	PINGRESP
	DISCONNECT
)

var packetNames = map[uint8]string{
	1:  "CONNECT",
	2:  "CONNACK",
	3:  "PUBLISH",
	4:  "PUBACK",
	5:  "PUBREC",
	6:  "PUBREL",
	7:  "PUBCOMP",
	8:  "SUBSCRIBE",
	9:  "SUBACK",
	10: "UNSUBSCRIBE",
	11: "UNSUBACK",
	12: "PINGREQ",
	13: "PINGRESP",
	14: "DISCONNECT",
}

type FH struct {
	T byte
	D bool
	Q byte
	R bool
	L int
}

func maskBytes(key []byte, pos int, b []byte) int {
	if len(b) < 2*wordSize {
		for i := range b {
			b[i] ^= key[pos&3]
			pos++
		}
		return pos & 3
	}
	if n := int(uintptr(unsafe.Pointer(&b[0]))) % wordSize; n != 0 {
		n = wordSize - n
		for i := range b[:n] {
			b[i] ^= key[pos&3]
			pos++
		}
		b = b[n:]
	}
	var k [wordSize]byte
	for i := range k {
		k[i] = key[(pos+i)&3]
	}
	kw := *(*uintptr)(unsafe.Pointer(&k))
	n := (len(b) / wordSize) * wordSize
	for i := 0; i < n; i += wordSize {
		*(*uintptr)(unsafe.Pointer(uintptr(unsafe.Pointer(&b[0])) + uintptr(i))) ^= kw
	}
	b = b[n:]
	for i := range b {
		b[i] ^= key[pos&3]
		pos++
	}
	return pos & 3
}

func (fh FH) String() string {
	return fmt.Sprintf("Q:%d L:%d", fh.Q, fh.L)
}

func read2(r *bufio.Reader) (int, error) {
	a, err := r.ReadByte()
	if err != nil {
		return 0, err
	}
	b, err := r.ReadByte()
	if err != nil {
		return 0, err
	}
	return int(a)<<8 | int(b), nil
}

func read8(r *bufio.Reader) (int, error) {
	if _, err := r.ReadByte(); err != nil {
		return 0, err
	}
	if _, err := r.ReadByte(); err != nil {
		return 0, err
	}
	if _, err := r.ReadByte(); err != nil {
		return 0, err
	}
	if _, err := r.ReadByte(); err != nil {
		return 0, err
	}
	a, err := r.ReadByte()
	if err != nil {
		return 0, err
	}
	b, err := r.ReadByte()
	if err != nil {
		return 0, err
	}
	c, err := r.ReadByte()
	if err != nil {
		return 0, err
	}
	d, err := r.ReadByte()
	if err != nil {
		return 0, err
	}
	return int(a)<<24 | int(b)<<16 | int(c)<<8 | int(d), nil
}

func readWebsocketPacket(r *bufio.Reader) (cp CP, err error) {
	_, err = r.ReadByte()
	if err != nil {
		return nil, err
	}
	second, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	flag := byte(second & 127)
	if (second >> 7) > 0 {
		var contentLength int
		switch flag {
		case 126:
			contentLength, err = read2(r)
			if err != nil {
				return nil, err
			}
		case 127:
			contentLength, err = read8(r)
			if err != nil {
				return nil, err
			}
		default:
			contentLength = int(flag)
		}
		mask := make([]byte, 4)
		_, err := io.ReadFull(r, mask)
		if err != nil {
			return nil, err
		}
		payload := make([]byte, contentLength)
		_, err = io.ReadFull(r, payload)
		if err != nil {
			return nil, err
		}
		maskBytes(mask, 0, payload)
		br := getBufioReader(bytes.NewReader(payload))
		defer putBufioReader(br)
		cp, err = readPacket(br)
		return cp, err
	} else {
		switch flag {
		case 126:
			if _, err := read2(r); err != nil {
				return nil, err
			}
		case 127:
			if _, err := read8(r); err != nil {
				return nil, err
			}
		default:
		}
		cp, err = readPacket(r)
		return cp, err
	}
}

func readPacket(r *bufio.Reader) (cp CP, err error) {
	first, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	fh := FH{
		T: first >> 4,
		D: (first>>3)&0x01 > 0,
		Q: (first >> 1) & 0x03,
		R: first&0x01 > 0,
	}
	err = fh.decodeLength(r)
	if err != nil {
		return nil, err
	}
	cp = newCPByFH(fh)
	if cp == nil {
		return nil, errors.New("Bad Data")
	}
	if fh.L == 0 {
		return cp, nil
	}
	err = cp.Decode(r)
	if err != nil {
		return nil, err
	}
	return cp, nil
}

func (fh *FH) first() byte {
	return fh.T<<4 | bTB(fh.D)<<3 | fh.Q<<1 | bTB(fh.R)
}

func (fh *FH) encode() []byte {
	w := bytePool.Get().(*bytes.Buffer)
	defer resetBytePool(w)
	w.WriteByte(fh.first())
	length := fh.L
	for {
		digit := byte(length % 128)
		length /= 128
		if length > 0 {
			digit |= 0x80
		}
		w.WriteByte(digit)
		if length == 0 {
			break
		}
	}
	return w.Bytes()
}

func bTB(b bool) byte {
	if b == true {
		return 1
	}
	return 0
}

func newCP(t byte) CP {
	fh := FH{T: t}
	if t == PUBREL || t == SUBSCRIBE || t == UNSUBSCRIBE {
		fh.Q = 1
	}
	return newCPByFH(fh)
}

func newCPByFH(fh FH) CP {
	switch fh.T {
	case CONNECT:
		return &ConnectP{FH: fh}
	case CONNACK:
		return &ConnackP{FH: fh}
	case PUBLISH:
		return &PublishP{FH: fh}
	case PUBACK:
		return &PubackP{FH: fh}
	case PUBREC:
		return &PubrecP{FH: fh}
	case PUBREL:
		return &PubrelP{FH: fh}
	case PUBCOMP:
		return &PubcompP{FH: fh}
	case SUBSCRIBE:
		return &SubscribeP{FH: fh}
	case SUBACK:
		return &SubackP{FH: fh}
	case UNSUBSCRIBE:
		return &UnsubscribeP{FH: fh}
	case UNSUBACK:
		return &UnsubackP{FH: fh}
	case PINGREQ:
		return &PingreqP{FH: fh}
	case PINGRESP:
		return &PingrespP{FH: fh}
	case DISCONNECT:
		return &DisconnectP{FH: fh}
	default:
		return nil
	}
}

func (fh *FH) decodeLength(r *bufio.Reader) error {
	var length, idx uint32
	for {
		digit, err := r.ReadByte()
		if err != nil {
			return err
		}
		length += (uint32(digit&0x7f) << idx)
		if (digit & 0x80) == 0 {
			break
		}
		idx += 7
	}
	fh.L = int(length)
	return nil
}

func decodeUint16(r *bufio.Reader) (uint16, error) {
	a, err := r.ReadByte()
	if err != nil {
		return 0, err
	}
	b, err := r.ReadByte()
	if err != nil {
		return 0, err
	}
	return uint16(a)<<8 | uint16(b), nil
}

func decodeString(r *bufio.Reader) (string, error) {
	l, err := decodeUint16(r)
	if err != nil {
		return "", err
	}
	/*
		w := bytePool.Get().(*bytes.Buffer)
		defer resetBytePool(w)
		_, err = io.CopyN(w, r, int64(l))
		return string(w.Bytes()), err
	*/
	b := make([]byte, l)
	_, err = io.ReadFull(r, b)
	return string(b), err
}

func decodeBytes(r *bufio.Reader) ([]byte, error) {
	l, err := decodeUint16(r)
	if err != nil {
		return []byte{}, err
	}
	/*
		w := bytePool.Get().(*bytes.Buffer)
		defer resetBytePool(w)
		_, err = io.CopyN(w, r, int64(l))
		return w.Bytes(), err
	*/
	b := make([]byte, l)
	_, err = io.ReadFull(r, b)
	return b, err
}

func resetBytePool(w *bytes.Buffer) {
	w.Reset()
	bytePool.Put(w)
}

/*******************************************************************************
connect packet
*******************************************************************************/
type ConnectP struct {
	FH
	ProtocolName    string
	ProtocolVersion byte
	CleanSession    bool
	WillFlag        bool
	WillQos         byte
	WillRetain      bool
	UsernameFlag    bool
	PasswordFlag    bool
	ReservedBit     byte
	Keepalive       uint16
	Key             string
	WillTopic       string
	WillMessage     []byte
	Username        string
	Password        []byte
}

func (p *ConnectP) String() string {
	format := "{%s %s:%d CleanSession:%t Keepalive:%d Key:%s}"
	return fmt.Sprintf(format, p.FH, p.ProtocolName, p.ProtocolVersion, p.CleanSession, p.Keepalive, p.Key)
}

func (p *ConnectP) Encode() []byte {
	w := bytePool.Get().(*bytes.Buffer)
	defer resetBytePool(w)
	length := 8 + len(p.ProtocolName) + len(p.Key)
	if p.WillFlag {
		length += (4 + len(p.WillTopic) + len(p.WillMessage))
	}
	if p.UsernameFlag {
		length += (2 + len(p.Username))
	}
	if p.PasswordFlag {
		length += (2 + len(p.Password))
	}
	p.L = length
	w.Write(p.FH.encode())
	w.Write([]byte{
		byte(len(p.ProtocolName) >> 8),
		byte(len(p.ProtocolName))})
	w.WriteString(p.ProtocolName)
	w.WriteByte(p.ProtocolVersion)
	w.WriteByte(bTB(p.CleanSession)<<1 |
		bTB(p.WillFlag)<<2 |
		p.WillQos<<3 |
		bTB(p.WillRetain)<<5 |
		bTB(p.PasswordFlag)<<6 |
		bTB(p.UsernameFlag)<<7)
	w.Write([]byte{
		byte(p.Keepalive >> 8),
		byte(p.Keepalive),
		byte(len(p.Key) >> 8),
		byte(len(p.Key))})
	w.WriteString(p.Key)
	if p.WillFlag {
		w.Write([]byte{
			byte(len(p.WillTopic) >> 8),
			byte(len(p.WillTopic))})
		w.WriteString(p.WillTopic)
		w.Write([]byte{
			byte(len(p.WillMessage) >> 8),
			byte(len(p.WillMessage))})
		w.Write(p.WillMessage)
	}
	if p.UsernameFlag {
		w.Write([]byte{
			byte(len(p.Username) >> 8),
			byte(len(p.Username))})
		w.WriteString(p.Username)
	}
	if p.PasswordFlag {
		w.Write([]byte{
			byte(len(p.Password) >> 8),
			byte(len(p.Password))})
		w.Write(p.Password)
	}
	return w.Bytes()
}

func (p *ConnectP) Decode(r *bufio.Reader) (err error) {
	p.ProtocolName, err = decodeString(r)
	if err != nil {
		return
	}
	p.ProtocolVersion, err = r.ReadByte()
	if err != nil {
		return
	}
	options, err := r.ReadByte()
	if err != nil {
		return
	}
	p.ReservedBit = options & 0x01
	p.CleanSession = (options>>1)&0x01 > 0
	p.WillFlag = (options>>2)&0x01 > 0
	p.WillQos = (options >> 3) & 0x03
	p.WillRetain = (options>>5)&0x01 > 0
	p.PasswordFlag = (options>>6)&0x01 > 0
	p.UsernameFlag = (options>>7)&0x01 > 0

	p.Keepalive, err = decodeUint16(r)
	if err != nil {
		return
	}
	p.Key, err = decodeString(r)
	if err != nil {
		return
	}
	if p.WillFlag {
		p.WillTopic, err = decodeString(r)
		if err != nil {
			return
		}
		p.WillMessage, err = decodeBytes(r)
		if err != nil {
			return
		}
	}
	if p.UsernameFlag {
		p.Username, err = decodeString(r)
		if err != nil {
			return
		}
	}
	if p.PasswordFlag {
		p.Password, err = decodeBytes(r)
		if err != nil {
			return
		}
	}
	return nil
}

func (p *ConnectP) DecodeBytes(b []byte) error {
	r := getBufioReader(bytes.NewBuffer(b))
	defer putBufioReader(r)
	return p.Decode(r)
}

/*******************************************************************************
connack packet
*******************************************************************************/
type ConnackP struct {
	FH
	SessionPresent bool
	ReturnCode     byte
}

func (p *ConnackP) String() string {
	return fmt.Sprintf("{%s SessionPresent:%t ReturnCode:%d}", p.FH, p.SessionPresent, p.ReturnCode)
}

func (p *ConnackP) Encode() []byte {
	w := bytePool.Get().(*bytes.Buffer)
	defer resetBytePool(w)
	p.L = 2
	w.Write(p.FH.encode())
	w.WriteByte(bTB(p.SessionPresent))
	w.WriteByte(p.ReturnCode)
	return w.Bytes()
}

func (p *ConnackP) Decode(r *bufio.Reader) error {
	i, err := r.ReadByte()
	if err != nil {
		return err
	}
	p.SessionPresent = i&0x01 > 0
	p.ReturnCode, err = r.ReadByte()
	return err
}

func (p *ConnackP) DecodeBytes(b []byte) error {
	r := getBufioReader(bytes.NewBuffer(b))
	defer putBufioReader(r)
	return p.Decode(r)
}

/*******************************************************************************
disconnect packet
*******************************************************************************/
type DisconnectP struct {
	FH
}

func (p *DisconnectP) String() string {
	return fmt.Sprintf("{%s}", p.FH)
}

func (p *DisconnectP) Encode() []byte {
	return p.FH.encode()
}

func (p *DisconnectP) Decode(r *bufio.Reader) error {
	return nil
}

func (p *DisconnectP) DecodeBytes(b []byte) error {
	return nil
}

/*******************************************************************************
pingreq packet
*******************************************************************************/
type PingreqP struct {
	FH
}

func (p *PingreqP) String() string {
	return fmt.Sprintf("{%s}", p.FH)
}

func (p *PingreqP) Encode() []byte {
	return p.FH.encode()
}

func (p *PingreqP) Decode(r *bufio.Reader) error {
	return nil
}

func (p *PingreqP) DecodeBytes(b []byte) error {
	return nil
}

/*******************************************************************************
pingresp packet
*******************************************************************************/
type PingrespP struct {
	FH
}

func (p *PingrespP) String() string {
	return fmt.Sprintf("{%s}", p.FH)
}

func (p *PingrespP) Encode() []byte {
	return []byte{208, 0}
}

func (p *PingrespP) Decode(r *bufio.Reader) error {
	return nil
}

func (p *PingrespP) DecodeBytes(b []byte) error {
	return nil
}

/*******************************************************************************
puback packet
*******************************************************************************/
type PubackP struct {
	FH
	MID uint16
}

var pubackH []byte = []byte{64, 2}

func (p *PubackP) String() string {
	return fmt.Sprintf("{%s MID:%d}", p.FH, p.MID)
}

func (p *PubackP) Encode() []byte {
	return []byte{64, 2, byte(p.MID >> 8), byte(p.MID)}
}

func (p *PubackP) Decode(r *bufio.Reader) (err error) {
	p.MID, err = decodeUint16(r)
	return
}

func (p *PubackP) DecodeBytes(b []byte) error {
	r := getBufioReader(bytes.NewBuffer(b))
	defer putBufioReader(r)
	return p.Decode(r)
}

/*******************************************************************************
pubcomp packet
*******************************************************************************/
type PubcompP struct {
	FH
	MID uint16
}

func (p *PubcompP) String() string {
	return fmt.Sprintf("{%s MID:%d}", p.FH, p.MID)
}

func (p *PubcompP) Encode() []byte {
	return []byte{112, 2, byte(p.MID >> 8), byte(p.MID)}
}

func (p *PubcompP) Decode(r *bufio.Reader) (err error) {
	p.MID, err = decodeUint16(r)
	return
}

func (p *PubcompP) DecodeBytes(b []byte) error {
	r := getBufioReader(bytes.NewBuffer(b))
	defer putBufioReader(r)
	return p.Decode(r)
}

/*******************************************************************************
publish packet
*******************************************************************************/
type PublishP struct {
	FH
	Topic   string
	MID     uint16
	Payload []byte
}

func (p *PublishP) String() string {
	length, idx := len(p.Payload), len(p.Payload)
	if length > 1000 {
		idx = 1000
	}
	format := "{%s Topic:%s MID:%d Payload:%s}"
	return fmt.Sprintf(format, p.FH, p.Topic, p.MID, string(p.Payload[:idx]))
}

func (p *PublishP) Encode() []byte {
	w := bytePool.Get().(*bytes.Buffer)
	defer resetBytePool(w)
	p.L = len(p.Payload) + len(p.Topic) + 2
	if p.Q > 0 {
		p.L += 2
	}
	w.Write(p.FH.encode())
	w.Write([]byte{
		byte(len(p.Topic) >> 8),
		byte(len(p.Topic))})
	w.WriteString(p.Topic)
	if p.Q > 0 {
		w.Write([]byte{
			byte(p.MID >> 8),
			byte(p.MID)})
	}
	w.Write(p.Payload)
	return w.Bytes()
}

func (p *PublishP) Decode(r *bufio.Reader) (err error) {
	length := p.L
	p.Topic, err = decodeString(r)
	if err != nil {
		return
	}
	length -= 2
	length -= len(p.Topic)
	if p.Q > 0 {
		p.MID, err = decodeUint16(r)
		if err != nil {
			return
		}
		length -= 2
	}
	/*
		w := bytePool.Get().(*bytes.Buffer)
		defer resetBytePool(w)
		_, err = io.CopyN(w, r, int64(length))
		if err != nil {
			return
		}
		p.Payload = w.Bytes()
	*/
	p.Payload = make([]byte, length)
	_, err = io.ReadFull(r, p.Payload)
	return
}

func (p *PublishP) DecodeBytes(b []byte) (err error) {
	r := getBufioReader(bytes.NewReader(b))
	defer putBufioReader(r)
	return p.Decode(r)
}

/*******************************************************************************
pubrec packet
*******************************************************************************/
type PubrecP struct {
	FH
	MID uint16
}

func (p *PubrecP) String() string {
	return fmt.Sprintf("{%s MID:%d}", p.FH, p.MID)
}

func (p *PubrecP) Encode() []byte {
	return []byte{80, 2, byte(p.MID >> 8), byte(p.MID)}
}

func (p *PubrecP) Decode(r *bufio.Reader) (err error) {
	p.MID, err = decodeUint16(r)
	return
}

func (p *PubrecP) DecodeBytes(b []byte) error {
	r := getBufioReader(bytes.NewBuffer(b))
	defer putBufioReader(r)
	return p.Decode(r)
}

/*******************************************************************************
pubrel packet
*******************************************************************************/
type PubrelP struct {
	FH
	MID uint16
}

var pubrelH []byte = []byte{98, 2}

func (p *PubrelP) String() string {
	return fmt.Sprintf("{%s MID:%d}", p.FH, p.MID)
}

func (p *PubrelP) Encode() []byte {
	return []byte{98, 2, byte(p.MID >> 8), byte(p.MID)}
}

func (p *PubrelP) Decode(r *bufio.Reader) (err error) {
	p.MID, err = decodeUint16(r)
	return
}

func (p *PubrelP) DecodeBytes(b []byte) error {
	r := getBufioReader(bytes.NewBuffer(b))
	defer putBufioReader(r)
	return p.Decode(r)
}

/*******************************************************************************
pubrel packet
*******************************************************************************/
type SubackP struct {
	FH
	MID         uint16
	GrantedQoss []byte
}

func (p *SubackP) String() string {
	return fmt.Sprintf("{%s MID:%d}", p.FH, p.MID)
}

func (p *SubackP) Encode() []byte {
	w := bytePool.Get().(*bytes.Buffer)
	defer resetBytePool(w)
	p.L = 2 + len(p.GrantedQoss)
	w.Write(p.FH.encode())
	w.Write([]byte{
		byte(p.MID >> 8),
		byte(p.MID)})
	w.Write(p.GrantedQoss)
	return w.Bytes()
}

func (p *SubackP) Decode(r *bufio.Reader) (err error) {
	length := p.L - 2
	p.MID, err = decodeUint16(r)
	if err != nil {
		return
	}
	p.GrantedQoss = make([]byte, length)
	_, err = io.ReadFull(r, p.GrantedQoss)
	return
}

func (p *SubackP) DecodeBytes(b []byte) error {
	r := getBufioReader(bytes.NewBuffer(b))
	defer putBufioReader(r)
	return p.Decode(r)
}

/*******************************************************************************
subscribe packet
*******************************************************************************/
type SubscribeP struct {
	FH
	MID    uint16
	Topics []string
	Qoss   []byte
}

func (p *SubscribeP) String() string {
	return fmt.Sprintf("{%s MID:%d Topics:%v}", p.FH, p.MID, p.Topics)
}

func (p *SubscribeP) Encode() []byte {
	w := bytePool.Get().(*bytes.Buffer)
	defer resetBytePool(w)
	p.L = 2 + len(p.Qoss)
	for _, topic := range p.Topics {
		p.L += len(topic)
		p.L += 2
	}
	w.Write(p.FH.encode())
	w.Write([]byte{
		byte(p.MID >> 8),
		byte(p.MID)})
	for i, topic := range p.Topics {
		w.Write([]byte{
			byte(len(topic) >> 8),
			byte(len(topic))})
		w.WriteString(topic)
		w.WriteByte(p.Qoss[i])
	}
	return w.Bytes()
}

func (p *SubscribeP) Decode(r *bufio.Reader) (err error) {
	length := p.L
	p.MID, err = decodeUint16(r)
	if err != nil {
		return
	}
	length -= 2
	for length > 0 {
		topic, err := decodeString(r)
		if err != nil {
			return err
		}
		p.Topics = append(p.Topics, topic)
		qos, err := r.ReadByte()
		if err != nil {
			return err
		}
		p.Qoss = append(p.Qoss, qos)
		length -= (len(topic) + 3)
	}
	return nil
}

func (p *SubscribeP) DecodeBytes(b []byte) error {
	r := getBufioReader(bytes.NewBuffer(b))
	defer putBufioReader(r)
	return p.Decode(r)
}

/*******************************************************************************
unsuback packet
*******************************************************************************/
type UnsubackP struct {
	FH
	MID uint16
}

var unsubackH []byte = []byte{176, 2}

func (p *UnsubackP) String() string {
	return fmt.Sprintf("{%s MID:%d}", p.FH, p.MID)
}

func (p *UnsubackP) Encode() []byte {
	return []byte{176, 2, byte(p.MID >> 8), byte(p.MID)}
}

func (p *UnsubackP) Decode(r *bufio.Reader) (err error) {
	p.MID, err = decodeUint16(r)
	return
}

func (p *UnsubackP) DecodeBytes(b []byte) error {
	r := getBufioReader(bytes.NewBuffer(b))
	defer putBufioReader(r)
	return p.Decode(r)
}

/*******************************************************************************
unsubscribe packet
*******************************************************************************/
type UnsubscribeP struct {
	FH
	MID    uint16
	Topics []string
}

func (p *UnsubscribeP) String() string {
	return fmt.Sprintf("{%s MID:%d Topics:%v}", p.FH, p.MID, p.Topics)
}

func (p *UnsubscribeP) Encode() []byte {
	w := bytePool.Get().(*bytes.Buffer)
	defer resetBytePool(w)
	p.L = 2
	for _, topic := range p.Topics {
		p.L += (len(topic) + 2)
	}
	w.Write(p.FH.encode())
	w.Write([]byte{
		byte(p.MID >> 8),
		byte(p.MID)})
	for _, topic := range p.Topics {
		w.Write([]byte{
			byte(len(topic) >> 8),
			byte(len(topic))})
		w.WriteString(topic)
	}
	return w.Bytes()
}

func (p *UnsubscribeP) Decode(r *bufio.Reader) (err error) {
	p.MID, err = decodeUint16(r)
	if err != nil {
		return
	}
	length := p.L - 2
	for length > 2 {
		topic, err := decodeString(r)
		if err != nil {
			return err
		}
		p.Topics = append(p.Topics, topic)
		length -= (len(topic) + 2)
	}
	if length != 0 {
		return errors.New("errorDecode")
	}
	return nil
}

func (p *UnsubscribeP) DecodeBytes(b []byte) error {
	r := getBufioReader(bytes.NewBuffer(b))
	defer putBufioReader(r)
	return p.Decode(r)
}
