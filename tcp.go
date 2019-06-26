package tcpraw

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

var (
	errOpNotImplemented = errors.New("operation not implemented")
	source              = rand.NewSource(time.Now().UnixNano())
)

// message represent a incoming packet with address
type message struct {
	bts  []byte
	addr net.Addr
}

// tcp flow information
type tcpFlow struct {
	handle       *pcap.Handle
	ready        chan struct{}
	seq          uint32
	ack          uint32
	linkLayer    gopacket.SerializableLayer // link layer header
	networkLayer gopacket.SerializableLayer // network layer header
}

// TCPConn defines a TCP-packet oriented connection
type TCPConn struct {
	server    bool // mark this connetion as tcp listener
	die       chan struct{}
	dieOnce   sync.Once
	socket    io.Closer
	localAddr *net.TCPAddr

	// gopacket
	handles      []*pcap.Handle
	packetSource *gopacket.PacketSource
	chMessage    chan message // incoming packets channel

	// important TCP header information
	flows     map[string]tcpFlow
	flowsLock sync.Mutex
}

func (conn *TCPConn) deleteflow(addr net.Addr) {
	key := addr.String()
	conn.flowsLock.Lock()
	delete(conn.flows, key)
	conn.flowsLock.Unlock()
}

func (conn *TCPConn) lockflow(addr net.Addr, f func(e *tcpFlow)) {
	key := addr.String()
	conn.flowsLock.Lock()
	e, ok := conn.flows[key]
	if !ok { // entry first visit
		e.ready = make(chan struct{})
	}
	f(&e)
	conn.flows[key] = e
	conn.flowsLock.Unlock()
}

func (conn *TCPConn) setttl(c net.Conn, ttl int) {
	if c.LocalAddr().(*net.TCPAddr).IP.To4() == nil {
		ipv6.NewConn(c).SetHopLimit(ttl)
	} else {
		ipv4.NewConn(c).SetTTL(ttl)
	}
}

// captureFlow capture each packets flowing based on rules of BPF
func (conn *TCPConn) captureFlow(handle *pcap.Handle) {
	source := gopacket.NewPacketSource(handle, handle.LinkType())

	go func() {
		for packet := range source.Packets() {
			transport := packet.TransportLayer().(*layers.TCP)

			// build address
			var ip []byte
			if layer := packet.Layer(layers.LayerTypeIPv4); layer != nil {
				network := layer.(*layers.IPv4)
				ip = make([]byte, len(network.SrcIP))
				copy(ip, network.SrcIP)
			} else if layer := packet.Layer(layers.LayerTypeIPv6); layer != nil {
				network := layer.(*layers.IPv6)
				ip = make([]byte, len(network.SrcIP))
				copy(ip, network.SrcIP)
			}
			addr := &net.TCPAddr{IP: ip, Port: int(transport.SrcPort)}

			if !transport.FIN && !transport.RST {
				conn.lockflow(addr, func(e *tcpFlow) {
					e.seq = transport.Ack // follow sequence number
					select {
					case <-e.ready:
					default:
						e.ack = transport.Seq
						e.handle = handle
						// link layer
						if layer := packet.Layer(layers.LayerTypeEthernet); layer != nil {
							ethLayer := layer.(*layers.Ethernet)
							e.linkLayer = &layers.Ethernet{
								EthernetType: ethLayer.EthernetType,
								SrcMAC:       ethLayer.DstMAC,
								DstMAC:       ethLayer.SrcMAC,
							}
						} else if layer := packet.Layer(layers.LayerTypeLoopback); layer != nil {
							loopLayer := layer.(*layers.Loopback)
							e.linkLayer = &layers.Loopback{Family: loopLayer.Family}
						} else {
							return
						}

						// network layer
						if layer := packet.Layer(layers.LayerTypeIPv4); layer != nil {
							network := layer.(*layers.IPv4)
							e.networkLayer = &layers.IPv4{
								SrcIP:    network.DstIP,
								DstIP:    network.SrcIP,
								Protocol: network.Protocol,
								Version:  network.Version,
								Id:       network.Id,
								Flags:    layers.IPv4DontFragment,
								TTL:      0x40,
							}
						} else if layer := packet.Layer(layers.LayerTypeIPv6); layer != nil {
							network := layer.(*layers.IPv6)
							e.networkLayer = &layers.IPv6{
								Version:    network.Version,
								NextHeader: network.NextHeader,
								SrcIP:      network.DstIP,
								DstIP:      network.SrcIP,
								HopLimit:   0x40,
							}
						} else {
							return
						}
						close(e.ready)
					}
				})
			}

			if transport.SYN {
				conn.lockflow(addr, func(e *tcpFlow) { e.ack++ })
			} else if transport.PSH {
				conn.lockflow(addr, func(e *tcpFlow) { e.ack += uint32(len(transport.Payload)) })
				select {
				case conn.chMessage <- message{transport.Payload, addr}:
				case <-conn.die:
					return
				}
			} else if transport.FIN || transport.RST {
				conn.deleteflow(addr)
			}
		}
	}()
}

// ReadFrom implements the PacketConn ReadFrom method.
func (conn *TCPConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	select {
	case <-conn.die:
		return 0, nil, io.EOF
	case packet := <-conn.chMessage:
		n = copy(p, packet.bts)
		return n, packet.addr, nil
	}
}

// WriteTo implements the PacketConn WriteTo method.
func (conn *TCPConn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	var ready chan struct{}
	conn.lockflow(addr, func(e *tcpFlow) { ready = e.ready })

	select {
	case <-conn.die:
		return 0, io.EOF
	case <-ready:
		tcpaddr, err := net.ResolveTCPAddr("tcp", addr.String())
		if err != nil {
			return 0, err
		}

		buf := gopacket.NewSerializeBuffer()
		opts := gopacket.SerializeOptions{
			FixLengths:       true,
			ComputeChecksums: true,
		}

		// fetch flow
		var flow tcpFlow
		conn.lockflow(addr, func(e *tcpFlow) { flow = *e })

		tcp := &layers.TCP{
			SrcPort: layers.TCPPort(conn.localAddr.Port),
			DstPort: layers.TCPPort(tcpaddr.Port),
			Window:  12580,
			Ack:     flow.ack,
			Seq:     flow.seq,
			PSH:     true,
			ACK:     true,
		}

		tcp.SetNetworkLayerForChecksum(flow.networkLayer.(gopacket.NetworkLayer))

		payload := gopacket.Payload(p)

		gopacket.SerializeLayers(buf, opts, flow.linkLayer, flow.networkLayer, tcp, payload)
		if err := flow.handle.WritePacketData(buf.Bytes()); err != nil {
			return 0, err
		}

		conn.lockflow(addr, func(e *tcpFlow) { e.seq += uint32(len(p)) })
		return len(p), nil
	}
}

// Close closes the connection.
func (conn *TCPConn) Close() error {
	var err error
	conn.dieOnce.Do(func() {
		close(conn.die)
		for k := range conn.handles {
			conn.handles[k].Close()
		}

		// recover ttl before close, so it can say goodbye
		if c, ok := conn.socket.(*net.TCPConn); ok {
			conn.setttl(c, 64)
		}
		err = conn.socket.Close()
	})
	return err
}

// LocalAddr returns the local network address.
func (conn *TCPConn) LocalAddr() net.Addr { return conn.localAddr }

// SetDeadline implements the Conn SetDeadline method.
func (conn *TCPConn) SetDeadline(t time.Time) error { return errOpNotImplemented }

// SetReadDeadline implements the Conn SetReadDeadline method.
func (conn *TCPConn) SetReadDeadline(t time.Time) error { return errOpNotImplemented }

// SetWriteDeadline implements the Conn SetWriteDeadline method.
func (conn *TCPConn) SetWriteDeadline(t time.Time) error { return errOpNotImplemented }

// Dial connects to the remote TCP port,
// and returns a single packet-oriented connection
func Dial(network, address string) (*TCPConn, error) {
	// remote address resolve
	raddr, err := net.ResolveTCPAddr(network, address)
	if err != nil {
		return nil, err
	}

	// create a dummy UDP socket, to get routing information
	dummy, err := net.Dial("udp", address)
	if err != nil {
		return nil, err
	}

	// get iface name from the dummy connection, eg. eth0, lo0
	ifaces, err := pcap.FindAllDevs()
	if err != nil {
		return nil, err
	}

	var ifaceName string
	for _, iface := range ifaces {
		for _, addr := range iface.Addresses {
			if addr.IP.Equal(dummy.LocalAddr().(*net.UDPAddr).IP) {
				ifaceName = iface.Name
			}
		}
	}
	if ifaceName == "" {
		return nil, errors.New("cannot find correct interface")
	}

	// pcap init
	handle, err := pcap.OpenLive(ifaceName, 65536, true, time.Second)
	if err != nil {
		return nil, err
	}

	// TCP local address reuses the same address from UDP
	laddr, err := net.ResolveTCPAddr(network, dummy.LocalAddr().String())
	if err != nil {
		return nil, err
	}
	dummy.Close()

	// apply filter for incoming data
	filter := fmt.Sprintf("tcp and dst host %v and dst port %v and src host %v and src port %v", laddr.IP, laddr.Port, raddr.IP, raddr.Port)
	if err := handle.SetBPFFilter(filter); err != nil {
		return nil, err
	}

	// create an established tcp connection
	// will hack this tcp connection for packet transmission
	tcpconn, err := net.DialTCP(network, laddr, raddr)
	if err != nil {
		return nil, err
	}

	// prevent tcpconn from sending ACKs

	// fields
	conn := new(TCPConn)
	conn.server = false
	conn.die = make(chan struct{})
	conn.flows = make(map[string]tcpFlow)
	conn.handles = []*pcap.Handle{handle}
	conn.socket = tcpconn
	conn.localAddr = tcpconn.LocalAddr().(*net.TCPAddr)
	conn.chMessage = make(chan message)
	conn.captureFlow(handle)
	conn.setttl(tcpconn, 0)

	// discards data flow on tcp conn, to keep the window slides
	go io.Copy(ioutil.Discard, tcpconn)

	return conn, nil
}

// Listen acts like net.ListenTCP,
// and returns a single packet-oriented connection
func Listen(network, address string) (*TCPConn, error) {
	laddr, err := net.ResolveTCPAddr(network, address)
	if err != nil {
		return nil, err
	}

	// get iface name from the dummy connection, eg. eth0, lo0
	ifaces, err := pcap.FindAllDevs()
	if err != nil {
		return nil, err
	}

	var handles []*pcap.Handle
	if laddr.IP == nil || laddr.IP.IsUnspecified() { // if address is not specified, capture on all iface
		for _, iface := range ifaces {
			if len(iface.Addresses) > 0 {
				// try open on all nics
				if handle, err := pcap.OpenLive(iface.Name, 65536, true, time.Second); err == nil {
					// apply filter
					filter := fmt.Sprintf("tcp and dst port %v", laddr.Port)
					if err := handle.SetBPFFilter(filter); err != nil {
						return nil, err
					}

					handles = append(handles, handle)
				}
			}
		}
		if len(handles) == 0 {
			return nil, errors.New("cannot find any interface")
		}
	} else {
		var ifaceName string
		for _, iface := range ifaces {
			for _, addr := range iface.Addresses {
				if addr.IP.Equal(laddr.IP) {
					ifaceName = iface.Name
				}
			}
		}
		if ifaceName == "" {
			return nil, errors.New("cannot find correct interface")
		}
		// pcap init
		handle, err := pcap.OpenLive(ifaceName, 65536, true, time.Second)
		if err != nil {
			return nil, err
		}

		// apply filter
		filter := fmt.Sprintf("tcp and dst host %v and dst port %v", laddr.IP, laddr.Port)
		if err := handle.SetBPFFilter(filter); err != nil {
			return nil, err
		}
		handles = []*pcap.Handle{handle}
	}

	// start listening
	l, err := net.ListenTCP(network, laddr)
	if err != nil {
		return nil, err
	}

	// fields
	conn := new(TCPConn)
	conn.server = true
	conn.handles = handles
	conn.flows = make(map[string]tcpFlow)
	conn.die = make(chan struct{})
	conn.socket = l
	conn.localAddr = l.Addr().(*net.TCPAddr)
	conn.chMessage = make(chan message)
	for k := range handles {
		conn.captureFlow(handles[k])
	}

	// discard everything in original connection
	go func() {
		for {
			tcpconn, err := l.Accept()
			if err != nil {
				return
			}

			// prevent conn from sending ACKs
			conn.setttl(tcpconn, 0)
			go io.Copy(ioutil.Discard, tcpconn)
		}
	}()

	return conn, nil
}
