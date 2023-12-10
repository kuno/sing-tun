package tun

import (
	"context"
	"net"
	"net/netip"
	"syscall"
	"time"

	"github.com/metacubex/sing-tun/internal/clashtcpip"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/control"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/udpnat"
)

type System struct {
	ctx                context.Context
	tun                Tun
	tunName            string
	mtu                uint32
	bufferSize         int
	handler            Handler
	logger             logger.Logger
	inet4Prefixes      []netip.Prefix
	inet6Prefixes      []netip.Prefix
	inet4ServerAddress netip.Addr
	inet4Address       netip.Addr
	inet6ServerAddress netip.Addr
	inet6Address       netip.Addr
	broadcastAddr      netip.Addr
	udpTimeout         int64
	tcpListener        net.Listener
	tcpListener6       net.Listener
	tcpPort            uint16
	tcpPort6           uint16
	tcpNat             *TCPNat
	udpNat             *udpnat.Service[netip.AddrPort]
	bindInterface      bool
	interfaceFinder    control.InterfaceFinder
	offload            bool
}

type Session struct {
	SourceAddress      netip.Addr
	DestinationAddress netip.Addr
	SourcePort         uint16
	DestinationPort    uint16
}

func NewSystem(options StackOptions) (Stack, error) {
	stack := &System{
		ctx:             options.Context,
		tun:             options.Tun,
		tunName:         options.TunOptions.Name,
		mtu:             options.TunOptions.MTU,
		bufferSize:      int(options.BufferSize()),
		udpTimeout:      options.UDPTimeout,
		handler:         options.Handler,
		logger:          options.Logger,
		inet4Prefixes:   options.TunOptions.Inet4Address,
		inet6Prefixes:   options.TunOptions.Inet6Address,
		broadcastAddr:   BroadcastAddr(options.TunOptions.Inet4Address),
		bindInterface:   options.ForwarderBindInterface,
		interfaceFinder: options.InterfaceFinder,
	}
	if len(options.TunOptions.Inet4Address) > 0 {
		if options.TunOptions.Inet4Address[0].Bits() == 32 {
			return nil, E.New("need one more IPv4 address in first prefix for system stack")
		}
		stack.inet4ServerAddress = options.TunOptions.Inet4Address[0].Addr()
		stack.inet4Address = stack.inet4ServerAddress.Next()
	}
	if len(options.TunOptions.Inet6Address) > 0 {
		if options.TunOptions.Inet6Address[0].Bits() == 128 {
			return nil, E.New("need one more IPv6 address in first prefix for system stack")
		}
		stack.inet6ServerAddress = options.TunOptions.Inet6Address[0].Addr()
		stack.inet6Address = stack.inet6ServerAddress.Next()
	}
	if !stack.inet4Address.IsValid() && !stack.inet6Address.IsValid() {
		return nil, E.New("missing interface address")
	}
	return stack, nil
}

func (s *System) Close() error {
	return common.Close(
		s.tcpListener,
		s.tcpListener6,
	)
}

func (s *System) Start() error {
	err := s.start()
	if err != nil {
		return err
	}
	go s.tunLoop()
	return nil
}

func (s *System) start() error {
	err := fixWindowsFirewall()
	if err != nil {
		return E.Cause(err, "fix windows firewall for system stack")
	}
	var listener net.ListenConfig
	if s.bindInterface {
		listener.Control = control.Append(listener.Control, func(network, address string, conn syscall.RawConn) error {
			err := control.BindToInterface(s.interfaceFinder, s.tunName, -1)(network, address, conn)
			if err != nil {
				s.logger.Warn("bind forwarder to interface: ", err)
			}
			return nil
		})
	}
	if s.inet4Address.IsValid() {
		tcpListener, err := listener.Listen(s.ctx, "tcp4", net.JoinHostPort(s.inet4ServerAddress.String(), "0"))
		if err != nil {
			return err
		}
		s.tcpListener = tcpListener
		s.tcpPort = M.SocksaddrFromNet(tcpListener.Addr()).Port
		go s.acceptLoop(tcpListener)
	}
	if s.inet6Address.IsValid() {
		tcpListener, err := listener.Listen(s.ctx, "tcp6", net.JoinHostPort(s.inet6ServerAddress.String(), "0"))
		if err != nil {
			return err
		}
		s.tcpListener6 = tcpListener
		s.tcpPort6 = M.SocksaddrFromNet(tcpListener.Addr()).Port
		go s.acceptLoop(tcpListener)
	}
	s.tcpNat = NewNat(s.ctx, time.Second*time.Duration(s.udpTimeout))
	s.udpNat = udpnat.New[netip.AddrPort](s.udpTimeout, s.handler)
	return nil
}

func (s *System) tunLoop() {
	if winTun, isWinTun := s.tun.(WinTun); isWinTun {
		s.wintunLoop(winTun)
		return
	}
	frontHeadroom := s.tun.FrontHeadroom()
	packetBuffer := make([]byte, s.bufferSize+frontHeadroom+PacketOffset)
	for {
		n, err := s.tun.Read(packetBuffer[frontHeadroom:])
		if err != nil {
			if E.IsClosed(err) {
				return
			}
			s.logger.Error(E.Cause(err, "read packet"))
		}
		if n < clashtcpip.IPv4PacketMinLength {
			continue
		}
		rawPacket := packetBuffer[:frontHeadroom+n]
		packet := packetBuffer[frontHeadroom+PacketOffset : frontHeadroom+n]
		switch ipVersion := packet[0] >> 4; ipVersion {
		case 4:
			err = s.processIPv4(rawPacket, packet)
		case 6:
			err = s.processIPv6(rawPacket, packet)
		default:
			err = E.New("ip: unknown version: ", ipVersion)
		}
		if err != nil {
			s.logger.Trace(err)
		}
	}
}

func (s *System) wintunLoop(winTun WinTun) {
	for {
		packet, release, err := winTun.ReadPacket()
		if err != nil {
			return
		}
		if len(packet) < clashtcpip.IPv4PacketMinLength {
			release()
			continue
		}
		switch ipVersion := packet[0] >> 4; ipVersion {
		case 4:
			err = s.processIPv4(packet, packet)
		case 6:
			err = s.processIPv6(packet, packet)
		default:
			err = E.New("ip: unknown version: ", ipVersion)
		}
		if err != nil {
			s.logger.Trace(err)
		}
		release()
	}
}

func (s *System) acceptLoop(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		connPort := M.SocksaddrFromNet(conn.RemoteAddr()).Port
		session := s.tcpNat.LookupBack(connPort)
		if session == nil {
			s.logger.Trace(E.New("unknown session with port ", connPort))
			continue
		}
		destination := M.SocksaddrFromNetIP(session.Destination)
		if destination.Addr.Is4() {
			for _, prefix := range s.inet4Prefixes {
				if prefix.Contains(destination.Addr) {
					destination.Addr = netip.AddrFrom4([4]byte{127, 0, 0, 1})
					break
				}
			}
		} else {
			for _, prefix := range s.inet6Prefixes {
				if prefix.Contains(destination.Addr) {
					destination.Addr = netip.AddrFrom16([16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1})
					break
				}
			}
		}
		go func() {
			_ = s.handler.NewConnection(s.ctx, conn, M.Metadata{
				Source:      M.SocksaddrFromNetIP(session.Source),
				Destination: destination,
			})
			if tcpConn, isTCPConn := conn.(*net.TCPConn); isTCPConn {
				_ = tcpConn.SetLinger(0)
			}
			_ = conn.Close()
		}()
	}
}

func (s *System) processIPv4(rawPacket []byte, packet clashtcpip.IPv4Packet) error {
	destination := packet.DestinationIP()
	if destination == s.broadcastAddr || !destination.IsGlobalUnicast() {
		return common.Error(s.tun.Write(rawPacket))
	}
	switch packet.Protocol() {
	case clashtcpip.TCP:
		return s.processIPv4TCP(rawPacket, packet, packet.Payload())
	case clashtcpip.UDP:
		return s.processIPv4UDP(rawPacket, packet, packet.Payload())
	case clashtcpip.ICMP:
		return s.processIPv4ICMP(rawPacket, packet, packet.Payload())
	default:
		return common.Error(s.tun.Write(rawPacket))
	}
}

func (s *System) processIPv6(rawPacket []byte, packet clashtcpip.IPv6Packet) error {
	if !packet.DestinationIP().IsGlobalUnicast() {
		return common.Error(s.tun.Write(rawPacket))
	}
	switch packet.Protocol() {
	case clashtcpip.TCP:
		return s.processIPv6TCP(rawPacket, packet, packet.Payload())
	case clashtcpip.UDP:
		return s.processIPv6UDP(rawPacket, packet, packet.Payload())
	case clashtcpip.ICMPv6:
		return s.processIPv6ICMP(rawPacket, packet, packet.Payload())
	default:
		return common.Error(s.tun.Write(rawPacket))
	}
}

func (s *System) processIPv4TCP(rawPacket []byte, packet clashtcpip.IPv4Packet, header clashtcpip.TCPPacket) error {
	source := netip.AddrPortFrom(packet.SourceIP(), header.SourcePort())
	destination := netip.AddrPortFrom(packet.DestinationIP(), header.DestinationPort())
	if !destination.Addr().IsGlobalUnicast() {
		return common.Error(s.tun.Write(rawPacket))
	} else if source.Addr() == s.inet4ServerAddress && source.Port() == s.tcpPort {
		session := s.tcpNat.LookupBack(destination.Port())
		if session == nil {
			return E.New("ipv4: tcp: session not found: ", destination.Port())
		}
		packet.SetSourceIP(session.Destination.Addr())
		header.SetSourcePort(session.Destination.Port())
		packet.SetDestinationIP(session.Source.Addr())
		header.SetDestinationPort(session.Source.Port())
	} else {
		natPort := s.tcpNat.Lookup(source, destination)
		packet.SetSourceIP(s.inet4Address)
		header.SetSourcePort(natPort)
		packet.SetDestinationIP(s.inet4ServerAddress)
		header.SetDestinationPort(s.tcpPort)
	}
	header.ResetChecksum(packet.PseudoSum())
	packet.ResetChecksum()
	return common.Error(s.tun.Write(rawPacket))
}

func (s *System) processIPv6TCP(rawPacket []byte, packet clashtcpip.IPv6Packet, header clashtcpip.TCPPacket) error {
	source := netip.AddrPortFrom(packet.SourceIP(), header.SourcePort())
	destination := netip.AddrPortFrom(packet.DestinationIP(), header.DestinationPort())
	if !destination.Addr().IsGlobalUnicast() {
		return common.Error(s.tun.Write(rawPacket))
	} else if source.Addr() == s.inet6ServerAddress && source.Port() == s.tcpPort6 {
		session := s.tcpNat.LookupBack(destination.Port())
		if session == nil {
			return E.New("ipv6: tcp: session not found: ", destination.Port())
		}
		packet.SetSourceIP(session.Destination.Addr())
		header.SetSourcePort(session.Destination.Port())
		packet.SetDestinationIP(session.Source.Addr())
		header.SetDestinationPort(session.Source.Port())
	} else {
		natPort := s.tcpNat.Lookup(source, destination)
		packet.SetSourceIP(s.inet6Address)
		header.SetSourcePort(natPort)
		packet.SetDestinationIP(s.inet6ServerAddress)
		header.SetDestinationPort(s.tcpPort6)
	}
	header.ResetChecksum(packet.PseudoSum())
	packet.ResetChecksum()
	return common.Error(s.tun.Write(rawPacket))
}

func (s *System) processIPv4UDP(rawPacket []byte, packet clashtcpip.IPv4Packet, header clashtcpip.UDPPacket) error {
	if packet.Flags()&clashtcpip.FlagMoreFragment != 0 {
		return E.New("ipv4: fragment dropped")
	}
	if packet.FragmentOffset() != 0 {
		return E.New("ipv4: udp: fragment dropped")
	}
	if !header.Valid() {
		return E.New("ipv4: udp: invalid packet")
	}
	source := netip.AddrPortFrom(packet.SourceIP(), header.SourcePort())
	destination := netip.AddrPortFrom(packet.DestinationIP(), header.DestinationPort())
	if !destination.Addr().IsGlobalUnicast() {
		return common.Error(s.tun.Write(rawPacket))
	}
	data := buf.As(header.Payload())
	if data.Len() == 0 {
		return nil
	}
	metadata := M.Metadata{
		Source:      M.SocksaddrFromNetIP(source),
		Destination: M.SocksaddrFromNetIP(destination),
	}
	s.udpNat.NewPacket(s.ctx, source, data.ToOwned(), metadata, func(natConn N.PacketConn) N.PacketWriter {
		headerLen := packet.HeaderLen() + clashtcpip.UDPHeaderSize
		headerCopy := make([]byte, headerLen)
		copy(headerCopy, packet[:headerLen])
		return &systemUDPPacketWriter4{s.tun, s.tun.FrontHeadroom(), headerCopy, source}
	})
	return nil
}

func (s *System) processIPv6UDP(rawPacket []byte, packet clashtcpip.IPv6Packet, header clashtcpip.UDPPacket) error {
	if !header.Valid() {
		return E.New("ipv6: udp: invalid packet")
	}
	source := netip.AddrPortFrom(packet.SourceIP(), header.SourcePort())
	destination := netip.AddrPortFrom(packet.DestinationIP(), header.DestinationPort())
	if !destination.Addr().IsGlobalUnicast() {
		return common.Error(s.tun.Write(rawPacket))
	}
	data := buf.As(header.Payload())
	if data.Len() == 0 {
		return nil
	}
	metadata := M.Metadata{
		Source:      M.SocksaddrFromNetIP(source),
		Destination: M.SocksaddrFromNetIP(destination),
	}
	s.udpNat.NewPacket(s.ctx, source, data.ToOwned(), metadata, func(natConn N.PacketConn) N.PacketWriter {
		headerLen := len(packet) - int(header.Length()) + clashtcpip.UDPHeaderSize
		headerCopy := make([]byte, headerLen)
		copy(headerCopy, packet[:headerLen])
		return &systemUDPPacketWriter6{s.tun, s.tun.FrontHeadroom(), headerCopy, source}
	})
	return nil
}

func (s *System) processIPv4ICMP(rawPacket []byte, packet clashtcpip.IPv4Packet, header clashtcpip.ICMPPacket) error {
	if header.Type() != clashtcpip.ICMPTypePingRequest || header.Code() != 0 {
		return nil
	}
	header.SetType(clashtcpip.ICMPTypePingResponse)
	sourceAddress := packet.SourceIP()
	packet.SetSourceIP(packet.DestinationIP())
	packet.SetDestinationIP(sourceAddress)
	header.ResetChecksum()
	packet.ResetChecksum()
	return common.Error(s.tun.Write(rawPacket))
}

func (s *System) processIPv6ICMP(rawPacket []byte, packet clashtcpip.IPv6Packet, header clashtcpip.ICMPv6Packet) error {
	if header.Type() != clashtcpip.ICMPv6EchoRequest || header.Code() != 0 {
		return nil
	}
	header.SetType(clashtcpip.ICMPv6EchoReply)
	sourceAddress := packet.SourceIP()
	packet.SetSourceIP(packet.DestinationIP())
	packet.SetDestinationIP(sourceAddress)
	header.ResetChecksum(packet.PseudoSum())
	packet.ResetChecksum()
	return common.Error(s.tun.Write(rawPacket))
}

type systemUDPPacketWriter4 struct {
	tun           Tun
	frontHeadroom int
	header        []byte
	source        netip.AddrPort
}

func (w *systemUDPPacketWriter4) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	newPacket := buf.NewSize(w.frontHeadroom + len(w.header) + buffer.Len())
	defer newPacket.Release()
	newPacket.WriteZeroN(w.frontHeadroom)
	newPacket.Advance(w.frontHeadroom)
	newPacket.Write(w.header)
	newPacket.Write(buffer.Bytes())
	ipHdr := clashtcpip.IPv4Packet(newPacket.Bytes())
	ipHdr.SetTotalLength(uint16(newPacket.Len()))
	ipHdr.SetDestinationIP(ipHdr.SourceIP())
	ipHdr.SetSourceIP(destination.Addr)
	udpHdr := clashtcpip.UDPPacket(ipHdr.Payload())
	udpHdr.SetDestinationPort(udpHdr.SourcePort())
	udpHdr.SetSourcePort(destination.Port)
	udpHdr.SetLength(uint16(buffer.Len() + clashtcpip.UDPHeaderSize))
	udpHdr.ResetChecksum(ipHdr.PseudoSum())
	ipHdr.ResetChecksum()
	newPacket.Advance(-w.frontHeadroom)
	return common.Error(w.tun.Write(newPacket.Bytes()))
}

type systemUDPPacketWriter6 struct {
	tun           Tun
	frontHeadroom int
	header        []byte
	source        netip.AddrPort
}

func (w *systemUDPPacketWriter6) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	newPacket := buf.NewSize(w.frontHeadroom + len(w.header) + buffer.Len())
	defer newPacket.Release()
	newPacket.WriteZeroN(w.frontHeadroom)
	newPacket.Advance(w.frontHeadroom)
	newPacket.Write(w.header)
	newPacket.Write(buffer.Bytes())
	ipHdr := clashtcpip.IPv6Packet(newPacket.Bytes())
	udpLen := uint16(clashtcpip.UDPHeaderSize + buffer.Len())
	ipHdr.SetPayloadLength(udpLen)
	ipHdr.SetDestinationIP(ipHdr.SourceIP())
	ipHdr.SetSourceIP(destination.Addr)
	udpHdr := clashtcpip.UDPPacket(ipHdr.Payload())
	udpHdr.SetDestinationPort(udpHdr.SourcePort())
	udpHdr.SetSourcePort(destination.Port)
	udpHdr.SetLength(udpLen)
	udpHdr.ResetChecksum(ipHdr.PseudoSum())
	newPacket.Advance(-w.frontHeadroom)
	return common.Error(w.tun.Write(newPacket.Bytes()))
}
