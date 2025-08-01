package main

import (
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/shadowsocks/go-shadowsocks2/socks"
)

type mode int

const (
	remoteServer mode = iota
	relayClient
	socksClient
)

const udpBufSize = 64 * 1024

// Listen on laddr for UDP packets, encrypt and send to server to reach target.
func udpLocal(laddr, server, target string, shadow func(net.PacketConn) net.PacketConn) {
	srvAddr, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		logf("UDP server address error: %v", err)
		return
	}

	tgt := socks.ParseAddr(target)
	if tgt == nil {
		err = fmt.Errorf("invalid target address: %q", target)
		logf("UDP target address error: %v", err)
		return
	}

	lnAddr, err := net.ResolveUDPAddr("udp", laddr)
	if err != nil {
		logf("UDP listen address error: %v", err)
		return
	}

	c, err := net.ListenUDP("udp", lnAddr)
	if err != nil {
		logf("UDP local listen error: %v", err)
		return
	}
	defer c.Close()

	nm := newNATmap(config.UDPTimeout)
	buf := make([]byte, udpBufSize)
	copy(buf, tgt)

	logf("UDP tunnel %s <-> %s <-> %s", laddr, server, target)
	for {
		n, addr, err := c.ReadFrom(buf[len(tgt):])
		if err != nil {
			logf("UDP local read error: %v", err)
			continue
		}
		raddr, err := udpAddrToNetip(addr)
		if err != nil {
			logf("Address conversion failed: %v", err)
			continue
		}

		pc := nm.Get(raddr)
		if pc == nil {
			pc, err = net.ListenPacket("udp", "")
			if err != nil {
				logf("UDP local listen error: %v", err)
				continue
			}

			pc = shadow(pc)
			nm.Add(raddr, c, pc, relayClient)
		}

		_, err = pc.WriteTo(buf[:len(tgt)+n], srvAddr)
		if err != nil {
			logf("UDP local write error: %v", err)
			continue
		}
	}
}

// Listen on laddr for Socks5 UDP packets, encrypt and send to server to reach target.
func udpSocksLocal(laddr, server string, shadow func(net.PacketConn) net.PacketConn) {
	srvAddr, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		logf("UDP server address error: %v", err)
		return
	}

	lnAddr, err := net.ResolveUDPAddr("udp", laddr)
	if err != nil {
		logf("UDP listen address error: %v", err)
		return
	}

	c, err := net.ListenUDP("udp", lnAddr)
	if err != nil {
		logf("UDP local listen error: %v", err)
		return
	}
	defer c.Close()

	nm := newNATmap(config.UDPTimeout)
	buf := make([]byte, udpBufSize)

	for {
		n, addr, err := c.ReadFrom(buf)
		if err != nil {
			logf("UDP local read error: %v", err)
			continue
		}
		raddr, err := udpAddrToNetip(addr)
		if err != nil {
			logf("Address conversion failed: %v", err)
			continue
		}

		pc := nm.Get(raddr)
		if pc == nil {
			pc, err = net.ListenPacket("udp", "")
			if err != nil {
				logf("UDP local listen error: %v", err)
				continue
			}
			logf("UDP socks tunnel %s <-> %s <-> %s", laddr, server, socks.Addr(buf[3:]))
			pc = shadow(pc)
			nm.Add(raddr, c, pc, socksClient)
		}

		_, err = pc.WriteTo(buf[3:n], srvAddr)
		if err != nil {
			logf("UDP local write error: %v", err)
			continue
		}
	}
}

type UDPConn interface {
	net.PacketConn
	ReadFromUDPAddrPort([]byte) (int, netip.AddrPort, error)
	WriteToUDPAddrPort([]byte, netip.AddrPort) (int, error)
}

// Listen on addr for encrypted packets and basically do UDP NAT.
func udpRemote(addr string, shadow func(net.PacketConn) net.PacketConn) {
	nAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		logf("UDP server address error: %v", err)
		return
	}
	cc, err := net.ListenUDP("udp", nAddr)
	if err != nil {
		logf("UDP remote listen error: %v", err)
		return
	}
	defer cc.Close()
	pc := shadow(cc) // net.PacketConn
	nm := newNATmap(config.UDPTimeout)
	buf := make([]byte, udpBufSize)

	logf("listening UDP on %s", addr)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			logf("UDP remote read error: %v", err)
			continue
		}
		raddr, err := udpAddrToNetip(addr)
		if err != nil {
			logf("Address conversion failed: %v", err)
			continue
		}

		tgtAddr := socks.SplitAddr(buf[:n])
		if tgtAddr == nil {
			logf("failed to split target address from packet: %q", buf[:n])
			continue
		}

		tgtUDPAddr, err := net.ResolveUDPAddr("udp", tgtAddr.String())
		if err != nil {
			logf("failed to resolve target UDP address: %v", err)
			continue
		}

		payload := buf[len(tgtAddr):n]

		pc := nm.Get(raddr)
		if pc == nil {
			pc, err = net.ListenPacket("udp", "")
			if err != nil {
				logf("UDP remote listen error: %v", err)
				continue
			}

			nm.Add(raddr, pc, pc, remoteServer)
		}

		_, err = pc.WriteTo(payload, tgtUDPAddr) // accept only UDPAddr despite the signature
		if err != nil {
			logf("UDP remote write error: %v", err)
			continue
		}
	}
}

// Packet NAT table
type natmap struct {
	sync.RWMutex
	m       map[netip.AddrPort]net.PacketConn
	timeout time.Duration
}

func newNATmap(timeout time.Duration) *natmap {
	m := &natmap{}
	m.m = make(map[netip.AddrPort]net.PacketConn)
	m.timeout = timeout
	return m
}

func (m *natmap) Get(key netip.AddrPort) net.PacketConn {
	m.RLock()
	defer m.RUnlock()
	return m.m[key]
}

func (m *natmap) Set(key netip.AddrPort, pc net.PacketConn) {
	m.Lock()
	defer m.Unlock()

	m.m[key] = pc
}

func (m *natmap) Del(key netip.AddrPort) net.PacketConn {
	m.Lock()
	defer m.Unlock()

	pc, ok := m.m[key]
	if ok {
		delete(m.m, key)
		return pc
	}
	return nil
}

func (m *natmap) Add(peer netip.AddrPort, dst net.PacketConn, src net.PacketConn, role mode) {
	m.Set(peer, src)

	go func() {
		timedCopy(dst, peer, src, m.timeout, role)
		if pc := m.Del(peer); pc != nil {
			pc.Close()
		}
	}()
}

// copy from src to dst at target with read timeout
func timedCopy(dst net.PacketConn, target netip.AddrPort, src net.PacketConn, timeout time.Duration, role mode) error {
	buf := make([]byte, udpBufSize)

	udpTarget := &net.UDPAddr{
		IP:   target.Addr().AsSlice(),
		Port: int(target.Port()),
	}

	for {
		src.SetReadDeadline(time.Now().Add(timeout))
		n, addr, err := src.ReadFrom(buf)
		if err != nil {
			return err
		}

		switch role {
		case remoteServer:
			srcAddr := socks.ParseAddr(addr.String())
			copy(buf[len(srcAddr):], buf[:n])
			copy(buf, srcAddr)
			_, err = dst.WriteTo(buf[:len(srcAddr)+n], udpTarget)
		case relayClient:
			srcAddr := socks.SplitAddr(buf[:n])
			_, err = dst.WriteTo(buf[len(srcAddr):n], udpTarget)
		case socksClient:
			_, err = dst.WriteTo(append([]byte{0, 0, 0}, buf[:n]...), udpTarget)
		}

		if err != nil {
			return err
		}
	}
}

func udpAddrToNetip(addr net.Addr) (netip.AddrPort, error) {
	udp, ok := addr.(*net.UDPAddr)
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("not a UDPAddr")
	}
	ip, err := netip.ParseAddr(udp.IP.String())
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("invalid IP: %v", err)
	}
	if !ip.IsValid() {
		return netip.AddrPort{}, fmt.Errorf("invalid IP: %v", udp.IP)
	}
	return netip.AddrPortFrom(ip, uint16(udp.Port)), nil
}
