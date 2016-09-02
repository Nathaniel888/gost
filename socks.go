package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"github.com/ginuerzh/gosocks5"
	"github.com/golang/glog"
	//"os/exec"
	//"io"
	"io/ioutil"
	"net"
	"net/url"
	"strconv"
	"time"
)

const (
	MethodTLS     uint8 = 0x80 // extended method for tls
	MethodTLSAuth uint8 = 0x82 // extended method for tls+auth
)

const (
	CmdUdpTun uint8 = 0xf3 // extended method for udp over tcp
)

type clientSelector struct {
	methods []uint8
	user    *url.Userinfo
}

func (selector *clientSelector) Methods() []uint8 {
	return selector.methods
}

func (selector *clientSelector) Select(methods ...uint8) (method uint8) {
	return
}

func (selector *clientSelector) OnSelected(method uint8, conn net.Conn) (net.Conn, error) {
	switch method {
	case MethodTLS:
		conn = tls.Client(conn, &tls.Config{InsecureSkipVerify: true})

	case gosocks5.MethodUserPass, MethodTLSAuth:
		if method == MethodTLSAuth {
			conn = tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
		}

		var username, password string
		if selector.user != nil {
			username = selector.user.Username()
			password, _ = selector.user.Password()
		}

		req := gosocks5.NewUserPassRequest(gosocks5.UserPassVer, username, password)
		if err := req.Write(conn); err != nil {
			glog.V(LWARNING).Infoln("socks5 auth:", err)
			return nil, err
		}
		glog.V(LDEBUG).Infoln(req)

		resp, err := gosocks5.ReadUserPassResponse(conn)
		if err != nil {
			glog.V(LWARNING).Infoln("socks5 auth:", err)
			return nil, err
		}
		glog.V(LDEBUG).Infoln(resp)

		if resp.Status != gosocks5.Succeeded {
			return nil, gosocks5.ErrAuthFailure
		}
	case gosocks5.MethodNoAcceptable:
		return nil, gosocks5.ErrBadMethod
	}

	return conn, nil
}

type serverSelector struct {
	methods []uint8
	user    *url.Userinfo
	cert    tls.Certificate
}

func (selector *serverSelector) Methods() []uint8 {
	return selector.methods
}

func (selector *serverSelector) Select(methods ...uint8) (method uint8) {
	glog.V(LDEBUG).Infof("%d %d %v", gosocks5.Ver5, len(methods), methods)

	method = gosocks5.MethodNoAuth
	for _, m := range methods {
		if m == MethodTLS {
			method = m
			break
		}
	}

	// when user/pass is set, auth is mandatory
	if selector.user != nil {
		if method == gosocks5.MethodNoAuth {
			method = gosocks5.MethodUserPass
		}
		if method == MethodTLS {
			method = MethodTLSAuth
		}
	}

	return
}

func (selector *serverSelector) OnSelected(method uint8, conn net.Conn) (net.Conn, error) {
	glog.V(LDEBUG).Infof("%d %d", gosocks5.Ver5, method)

	switch method {
	case MethodTLS:
		conn = tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{selector.cert}})

	case gosocks5.MethodUserPass, MethodTLSAuth:
		if method == MethodTLSAuth {
			conn = tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{selector.cert}})
		}

		req, err := gosocks5.ReadUserPassRequest(conn)
		if err != nil {
			glog.V(LWARNING).Infoln("[socks5-auth]", err)
			return nil, err
		}
		glog.V(LDEBUG).Infoln("[socks5]", req.String())

		var username, password string
		if selector.user != nil {
			username = selector.user.Username()
			password, _ = selector.user.Password()
		}

		if (username != "" && req.Username != username) || (password != "" && req.Password != password) {
			resp := gosocks5.NewUserPassResponse(gosocks5.UserPassVer, gosocks5.Failure)
			if err := resp.Write(conn); err != nil {
				glog.V(LWARNING).Infoln("[socks5-auth]", err)
				return nil, err
			}
			glog.V(LDEBUG).Infoln("[socks5]", resp)
			glog.V(LWARNING).Infoln("[socks5-auth] proxy authentication required")

			return nil, gosocks5.ErrAuthFailure
		}

		resp := gosocks5.NewUserPassResponse(gosocks5.UserPassVer, gosocks5.Succeeded)
		if err := resp.Write(conn); err != nil {
			glog.V(LWARNING).Infoln("[socks5-auth]", err)
			return nil, err
		}
		glog.V(LDEBUG).Infoln(resp)

	case gosocks5.MethodNoAcceptable:
		return nil, gosocks5.ErrBadMethod
	}

	return conn, nil
}

func handleSocks5Request(req *gosocks5.Request, conn net.Conn) {
	glog.V(LDEBUG).Infof("[socks5] %s -> %s\n%s", conn.RemoteAddr(), req.Addr, req)

	switch req.Cmd {
	case gosocks5.CmdConnect:
		glog.V(LINFO).Infof("[socks5-connect] %s - %s", conn.RemoteAddr(), req.Addr)

		tconn, err := Connect(req.Addr.String())
		if err != nil {
			glog.V(LWARNING).Infof("[socks5-connect] %s -> %s : %s", conn.RemoteAddr(), req.Addr, err)
			rep := gosocks5.NewReply(gosocks5.HostUnreachable, nil)
			if err := rep.Write(conn); err != nil {
				glog.V(LWARNING).Infof("[socks5-connect] %s <- %s : %s", conn.RemoteAddr(), req.Addr, err)
			} else {
				glog.V(LDEBUG).Infof("[socks5-connect] %s <- %s\n%s", conn.RemoteAddr(), req.Addr, rep)
			}
			return
		}
		defer tconn.Close()

		rep := gosocks5.NewReply(gosocks5.Succeeded, nil)
		if err := rep.Write(conn); err != nil {
			glog.V(LWARNING).Infof("[socks5-connect] %s <- %s : %s", conn.RemoteAddr(), req.Addr, err)
			return
		}
		glog.V(LDEBUG).Infof("[socks5-connect] %s <- %s\n%s", conn.RemoteAddr(), req.Addr, rep)

		glog.V(LINFO).Infof("[socks5-connect] %s <-> %s", conn.RemoteAddr(), req.Addr)
		Transport(conn, tconn)
		glog.V(LINFO).Infof("[socks5-connect] %s >-< %s", conn.RemoteAddr(), req.Addr)
	case gosocks5.CmdBind:
		glog.V(LINFO).Infof("[socks5-bind] %s - %s", conn.RemoteAddr(), req.Addr)

		if len(forwardArgs) > 0 {
			forwardBind(req, conn)
		} else {
			tc := conn.(*net.TCPConn)
			tc.SetKeepAlive(true)
			tc.SetKeepAlivePeriod(time.Second * 60)

			serveBind(req, conn)
		}
	case gosocks5.CmdUdp, CmdUdpTun:
		// TODO: udp tunnel <-> forward chain
		glog.V(LINFO).Infof("[socks5-udp] %s - %s ASSOCIATE", conn.RemoteAddr(), req.Addr)
		uconn, err := net.ListenUDP("udp", nil)
		if err != nil {
			glog.V(LWARNING).Infof("[socks5-udp] %s -> %s : %s", conn.RemoteAddr(), req.Addr, err)

			rep := gosocks5.NewReply(gosocks5.Failure, nil)
			if err := rep.Write(conn); err != nil {
				glog.V(LWARNING).Infof("[socks5-udp] %s <- %s : %s", conn.RemoteAddr(), req.Addr, err)
			} else {
				glog.V(LDEBUG).Infof("[socks5-udp] %s <- %s\n%s", conn.RemoteAddr(), req.Addr, rep)
			}
			return
		}
		defer uconn.Close()

		addr := ToSocksAddr(uconn.LocalAddr())
		addr.Host, _, _ = net.SplitHostPort(conn.LocalAddr().String()) // BUG: when server has multi-interfaces, this may cause a mistake

		rep := gosocks5.NewReply(gosocks5.Succeeded, addr)
		if err := rep.Write(conn); err != nil {
			glog.V(LWARNING).Infof("[socks5-udp] %s <- %s : %s", conn.RemoteAddr(), req.Addr, err)
			return
		} else {
			glog.V(LDEBUG).Infof("[socks5-udp] %s <- %s\n%s", conn.RemoteAddr(), req.Addr, rep)
			glog.V(LINFO).Infof("[socks5-udp] %s -> %s LISTEN ON %s", conn.RemoteAddr(), req.Addr, addr)
		}

		var cc *UDPConn
		var dgram *gosocks5.UDPDatagram
		if req.Cmd == CmdUdpTun {
			dgram, err = gosocks5.ReadUDPDatagram(conn)
			if err != nil {
				glog.V(LWARNING).Infof("[socks5-udp] %s -> %s : %s", conn.RemoteAddr(), req.Addr, err)
				return
			}
			glog.V(LINFO).Infof("[socks5-udp] %s >>> %s, length %d", conn.RemoteAddr(), dgram.Header.Addr, len(dgram.Data))
			cc = Client(conn, nil)
		} else {
			b := udpPool.Get().([]byte)
			defer udpPool.Put(b)

			n, raddr, err := uconn.ReadFromUDP(b)
			if err != nil {
				glog.V(LWARNING).Infof("[socks5-udp] %s -> %s : %s", conn.RemoteAddr(), req.Addr, err)
				return
			}
			dgram, err = gosocks5.ReadUDPDatagram(bytes.NewReader(b[:n]))
			if err != nil {
				glog.V(LWARNING).Infof("[socks5-udp] %s -> %s : %s", conn.RemoteAddr(), req.Addr, err)
				return
			}
			glog.V(LINFO).Infof("[socks5-udp] %s >>> %s, length %d", raddr, dgram.Header.Addr, len(dgram.Data))
			cc = Client(uconn, raddr)
		}

		sc, err := createServerConn(uconn)
		if err != nil {
			glog.V(LWARNING).Infof("[socks5-udp] %s", err)
			return
		}
		defer sc.Close()

		if err = sc.WriteUDPTimeout(dgram, time.Second*90); err != nil {
			glog.V(LWARNING).Infoln("socks5 udp:", err)
			return
		}
		dgram, err = sc.ReadUDPTimeout(time.Second * 90)
		if err != nil {
			glog.V(LWARNING).Infoln("socks5 udp:", err)
			return
		}
		glog.V(LINFO).Infof("[socks5-udp] <<< %s, length %d", dgram.Header.Addr, len(dgram.Data))

		if err = cc.WriteUDPTimeout(dgram, time.Second*90); err != nil {
			glog.V(LWARNING).Infoln("socks5 udp:", err)
			return
		}

		if req.Cmd == gosocks5.CmdUdp {
			go TransportUDP(cc, sc)
			ioutil.ReadAll(conn) // wait for client exit
			glog.V(LINFO).Infoln("[udp] transfer done")
		} else {
			TransportUDP(cc, sc)
		}
	default:
		glog.V(LWARNING).Infoln("[socks5] Unrecognized request:", req.Cmd)
	}
}

func serveBind(req *gosocks5.Request, conn net.Conn) error {
	bindAddr, _ := net.ResolveTCPAddr("tcp", req.Addr.String())
	ln, err := net.ListenTCP("tcp", bindAddr)
	if err != nil {
		glog.V(LWARNING).Infof("[socks5-bind] %s -> %s : %s", conn.RemoteAddr(), req.Addr, err)
		if bindAddr != nil {
			ln, err = net.ListenTCP("tcp", nil)
		}
		if err != nil {
			glog.V(LWARNING).Infof("[socks5-bind] %s -> %s : %s", conn.RemoteAddr(), req.Addr, err)
			rep := gosocks5.NewReply(gosocks5.Failure, nil)
			if err := rep.Write(conn); err != nil {
				glog.V(LWARNING).Infof("[socks5-bind] %s <- %s : %s", conn.RemoteAddr(), req.Addr, err)
			} else {
				glog.V(LDEBUG).Infof("[socks5-bind] %s <- %s\n%s", conn.RemoteAddr(), req.Addr, rep)
			}
			return err
		}
	}

	addr := ToSocksAddr(ln.Addr())
	// Issue: may not reachable when host has two interfaces
	addr.Host, _, _ = net.SplitHostPort(conn.LocalAddr().String())

	rep := gosocks5.NewReply(gosocks5.Succeeded, addr)
	if err := rep.Write(conn); err != nil {
		glog.V(LWARNING).Infof("[socks5-bind] %s <- %s : %s", conn.RemoteAddr(), req.Addr, err)
		ln.Close()
		return err
	}
	glog.V(LDEBUG).Infof("[socks5-bind] %s <- %s\n%s", conn.RemoteAddr(), req.Addr, rep)
	glog.V(LINFO).Infof("[socks5-bind] %s - %s BIND ON %s OK", conn.RemoteAddr(), req.Addr, addr)

	lnChan := make(chan net.Conn, 1)
	go func() {
		defer close(lnChan)
		c, err := ln.AcceptTCP()
		if err != nil {
			return
		}
		lnChan <- c
	}()

	peerChan := make(chan error, 1)
	go func() {
		defer close(peerChan)
		_, err := ioutil.ReadAll(conn)
		if err != nil {
			if opErr, ok := err.(*net.OpError); ok && opErr.Timeout() {
				return
			}
			peerChan <- err
		}
	}()

	var pconn net.Conn

	for {
		select {
		case c := <-lnChan:
			ln.Close() // only accept one peer
			if c == nil {
				if err := gosocks5.NewReply(gosocks5.Failure, nil).Write(conn); err != nil {
					glog.V(LWARNING).Infoln("[socks5-bind] %s <- %s : %s", conn.RemoteAddr(), addr, err)
				}
				glog.V(LWARNING).Infof("[socks5-bind] %s >-< %s", conn.RemoteAddr(), addr)
				return errors.New("accept error")
			}
			// glog.V(LINFO).Infof("[socks5-bind] %s <- %s PEER %s ACCEPTED", conn.RemoteAddr(), req.Addr, c.RemoteAddr())
			// gosocks5.NewReply(gosocks5.Succeeded, ToSocksAddr(c.RemoteAddr())).Write(conn)
			pconn = c
			lnChan = nil
			ln = nil
			conn.SetReadDeadline(time.Now()) // timeout right now ,so we can break out of blocking
		case err := <-peerChan:
			if err != nil || pconn == nil {
				if ln != nil {
					ln.Close()
				}
				if pconn != nil {
					pconn.Close()
				}
				glog.V(LWARNING).Infof("[socks5-bind] %s >-< %s", conn.RemoteAddr(), addr)
				return err
			}
			goto out
		}
	}

out:
	defer pconn.Close()
	conn.SetReadDeadline(time.Time{})

	paddr := ToSocksAddr(pconn.RemoteAddr())
	glog.V(LINFO).Infof("[socks5-bind] %s <- %s PEER %s ACCEPTED", conn.RemoteAddr(), addr, paddr)

	rep = gosocks5.NewReply(gosocks5.Succeeded, paddr)
	if err := rep.Write(conn); err != nil {
		glog.V(LWARNING).Infof("[socks5 bind] %s <- %s : %s", conn.RemoteAddr(), addr, err)
		return err
	}
	glog.V(LDEBUG).Infof("[socks5 bind] %s <- %s\n%s", conn.RemoteAddr(), addr, rep)

	glog.V(LINFO).Infof("[socks5-bind] %s <-> %s", conn.RemoteAddr(), paddr)
	defer glog.V(LINFO).Infof("[socks5-bind] %s >-< %s", conn.RemoteAddr(), paddr)
	return Transport(conn, pconn)
}

func forwardBind(req *gosocks5.Request, conn net.Conn) error {
	fconn, _, err := forwardChain(forwardArgs...)
	if err != nil {
		glog.V(LWARNING).Infof("[socks5-bind] %s -> %s : %s", conn.RemoteAddr(), req.Addr, err)
		if fconn != nil {
			fconn.Close()
		}
		rep := gosocks5.NewReply(gosocks5.Failure, nil)
		if err := rep.Write(conn); err != nil {
			glog.V(LWARNING).Infof("[socks5-bind] %s <- %s : %s", conn.RemoteAddr(), req.Addr, err)
		} else {
			glog.V(LDEBUG).Infof("[socks5-bind] %s <- %s\n%s", conn.RemoteAddr(), req.Addr, rep)
		}
		return err
	}
	defer fconn.Close()

	if err := req.Write(fconn); err != nil {
		glog.V(LWARNING).Infoln("[socks5-bind] %s -> %s : %s", conn.RemoteAddr(), req.Addr, err)
		gosocks5.NewReply(gosocks5.Failure, nil).Write(conn)
		return err
	}
	glog.V(LDEBUG).Infof("[socks5-bind] %s -> %s\n%s", conn.RemoteAddr(), req.Addr, req)

	/*
		// first reply
		rep, err := peekReply(conn, fconn)
		if err != nil {
			glog.V(LWARNING).Infoln("[socks5] BIND forward", err)
			return err
		}
		glog.V(LINFO).Infoln("[socks5] BIND forward on", rep.Addr, "OK")

		// second reply
		rep, err = peekReply(conn, fconn)
		if err != nil {
			glog.V(LWARNING).Infoln("[socks5] BIND forward accept", err)
			return err
		}
		glog.V(LINFO).Infoln("[socks5] BIND forward accept", rep.Addr)
	*/
	glog.V(LINFO).Infof("[socks5-bind] %s <-> %s", conn.RemoteAddr(), req.Addr)
	defer glog.V(LINFO).Infof("[socks5-bind] %s >-< %s", conn.RemoteAddr(), req.Addr)
	return Transport(conn, fconn)
}

/*
func peekReply(dst, src net.Conn) (rep *gosocks5.Reply, err error) {
	rep, err = gosocks5.ReadReply(src)
	if err != nil {
		glog.V(LWARNING).Infof("[socks5-bind] FORWARD %s <- : %s", dst.RemoteAddr(), err)
		rep = gosocks5.NewReply(gosocks5.Failure, nil)
	}
	if err = rep.Write(dst); err != nil {
		return
	}
	glog.V(LDEBUG).Infof("[socks5-bind] FORWARD %s <-\n%s", dst.RemoteAddr(), rep)

	if rep.Rep != gosocks5.Succeeded {
		err = errors.New("Failure")
	}

	return
}
*/

func createServerConn(uconn *net.UDPConn) (c *UDPConn, err error) {
	if len(forwardArgs) == 0 {
		c = Server(uconn)
		return
	}

	fconn, _, err := forwardChain(forwardArgs...)
	if err != nil {
		if fconn != nil {
			fconn.Close()
		}
		return
	}
	glog.V(LINFO).Infoln("[udp] forward associate")

	req := gosocks5.NewRequest(CmdUdpTun, nil)
	if err = req.Write(fconn); err != nil {
		fconn.Close()
		return
	}
	glog.V(LDEBUG).Infoln(req)

	rep, err := gosocks5.ReadReply(fconn)
	if err != nil {
		fconn.Close()
		return
	}
	glog.V(LDEBUG).Infoln(rep)
	if rep.Rep != gosocks5.Succeeded {
		fconn.Close()
		return nil, errors.New("Failure")
	}
	glog.V(LINFO).Infoln("[udp] forward associate on", rep.Addr, "OK")

	c = Server(fconn)
	return
}

func ToSocksAddr(addr net.Addr) *gosocks5.Addr {
	host := ""
	port := 0
	if addr != nil {
		h, p, _ := net.SplitHostPort(addr.String())
		host = h
		port, _ = strconv.Atoi(p)
	}
	return &gosocks5.Addr{
		Type: gosocks5.AddrIPv4,
		Host: host,
		Port: uint16(port),
	}
}

func PipeUDP(src, dst *UDPConn, ch chan<- error) {
	var err error

	for {
		var dgram *gosocks5.UDPDatagram
		dgram, err = src.ReadUDP()
		if err != nil {
			break
		}
		if src.isClient {
			glog.V(LDEBUG).Infof("[udp] -> %s, length %d", dgram.Header.Addr, len(dgram.Data))
		} else {
			glog.V(LDEBUG).Infof("[udp] <- %s, length %d", dgram.Header.Addr, len(dgram.Data))
		}
		if err = dst.WriteUDP(dgram); err != nil {
			break
		}
	}

	ch <- err
	close(ch)
}

func TransportUDP(cc, sc *UDPConn) (err error) {
	rChan := make(chan error, 1)
	wChan := make(chan error, 1)

	go PipeUDP(cc, sc, wChan)
	go PipeUDP(sc, cc, rChan)

	select {
	case err = <-wChan:
		// glog.V(LDEBUG).Infoln("w exit", err)
	case err = <-rChan:
		// glog.V(LDEBUG).Infoln("r exit", err)
	}

	return
}
