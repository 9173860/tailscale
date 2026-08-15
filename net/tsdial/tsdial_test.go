// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package tsdial

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/gaissmai/bart"
)

func TestUserDialPlan(t *testing.T) {
	tests := []struct {
		name           string
		addr           string
		routes         map[netip.Prefix]bool // nil means no routes configured
		useNetstackFor func(netip.Addr) bool // nil means not set
		wantVia        bool
		wantAddr       netip.AddrPort
	}{
		{
			name:     "loopback_no_routes",
			addr:     "127.0.0.1:8080",
			wantVia:  false,
			wantAddr: netip.MustParseAddrPort("127.0.0.1:8080"),
		},
		{
			name:     "loopback_v6_no_routes",
			addr:     "[::1]:8080",
			wantVia:  false,
			wantAddr: netip.MustParseAddrPort("[::1]:8080"),
		},
		{
			name: "tailscale_ip_in_routes",
			addr: "100.64.1.1:22",
			routes: map[netip.Prefix]bool{
				netip.MustParsePrefix("100.64.0.0/10"): true,
			},
			wantVia:  true,
			wantAddr: netip.MustParseAddrPort("100.64.1.1:22"),
		},
		{
			name: "non_tailscale_ip_in_local_routes",
			addr: "10.0.0.5:80",
			routes: map[netip.Prefix]bool{
				netip.MustParsePrefix("100.64.0.0/10"): true,
				netip.MustParsePrefix("10.0.0.0/8"):    false, // local route
			},
			wantVia:  false,
			wantAddr: netip.MustParseAddrPort("10.0.0.5:80"),
		},
		{
			name: "loopback_with_routes_configured",
			addr: "127.0.0.1:3000",
			routes: map[netip.Prefix]bool{
				netip.MustParsePrefix("100.64.0.0/10"): true,
			},
			wantVia:  false,
			wantAddr: netip.MustParseAddrPort("127.0.0.1:3000"),
		},
		{
			name: "netstack_for_ip",
			addr: "100.100.100.100:53",
			useNetstackFor: func(ip netip.Addr) bool {
				return ip == netip.MustParseAddr("100.100.100.100")
			},
			wantVia:  true,
			wantAddr: netip.MustParseAddrPort("100.100.100.100:53"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Dialer{}
			if tt.routes != nil {
				rt := &bart.Table[bool]{}
				for pfx, v := range tt.routes {
					rt.Insert(pfx, v)
				}
				d.routes.Store(rt)
			}
			d.UseNetstackForIP = tt.useNetstackFor

			ipp, viaTailscale, err := d.UserDialPlan(context.Background(), "tcp", tt.addr)
			if err != nil {
				t.Fatalf("UserDialPlan: %v", err)
			}
			if viaTailscale != tt.wantVia {
				t.Errorf("viaTailscale = %v, want %v", viaTailscale, tt.wantVia)
			}
			if ipp != tt.wantAddr {
				t.Errorf("addr = %v, want %v", ipp, tt.wantAddr)
			}
		})
	}
}

func TestSystemDialCloseWritePreservesTracking(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	serverConn := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			serverConn <- c
		}
	}()

	d := &Dialer{}
	d.SetSystemDialerForTest((&net.Dialer{}).DialContext)
	c, err := d.SystemDial(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	peer := <-serverConn
	defer peer.Close()

	cw, ok := c.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("SystemDial connection does not expose CloseWrite")
	}
	if err := cw.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	if got := len(d.activeSysConns); got != 1 {
		t.Fatalf("active system connections after CloseWrite = %d, want 1", got)
	}

	if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if n, err := peer.Read(buf); n != 0 || err != io.EOF {
		t.Fatalf("peer Read after CloseWrite = %d, %v; want 0, EOF", n, err)
	}
}

func TestSystemDialCloseWriteUnsupported(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	d := &Dialer{}
	d.SetSystemDialerForTest(func(context.Context, string, string) (net.Conn, error) {
		return a, nil
	})
	c, err := d.SystemDial(context.Background(), "tcp", "unused")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cw, ok := c.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("SystemDial connection does not expose CloseWrite")
	}
	if err := cw.CloseWrite(); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("CloseWrite error = %v, want errors.ErrUnsupported", err)
	}
	if got := len(d.activeSysConns); got != 1 {
		t.Fatalf("active system connections after unsupported CloseWrite = %d, want 1", got)
	}
}
