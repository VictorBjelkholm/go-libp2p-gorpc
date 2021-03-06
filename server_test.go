package rpc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	logging "github.com/ipfs/go-log"
	crypto "github.com/libp2p/go-libp2p-crypto"
	host "github.com/libp2p/go-libp2p-host"
	peer "github.com/libp2p/go-libp2p-peer"
	peerstore "github.com/libp2p/go-libp2p-peerstore"
	swarm "github.com/libp2p/go-libp2p-swarm"
	basic "github.com/libp2p/go-libp2p/p2p/host/basic"
	multiaddr "github.com/multiformats/go-multiaddr"
)

func init() {
	logging.SetLogLevel("p2p-gorpc", "DEBUG")
	//logging.SetDebugLogging()
}

type Args struct {
	A, B int
}

type Quotient struct {
	Quo, Rem int
}

type ctxTracker struct {
	ctxMu sync.Mutex
	ctx   context.Context
}

func (ctxt *ctxTracker) setCtx(ctx context.Context) {
	ctxt.ctxMu.Lock()
	defer ctxt.ctxMu.Unlock()
	ctxt.ctx = ctx
}

func (ctxt *ctxTracker) cancelled() bool {
	ctxt.ctxMu.Lock()
	defer ctxt.ctxMu.Unlock()
	return ctxt.ctx.Err() != nil
}

type Arith struct {
	ctxTracker *ctxTracker
}

func (t *Arith) Multiply(ctx context.Context, args *Args, reply *int) error {
	*reply = args.A * args.B
	return nil
}

// This uses non pointer args
func (t *Arith) Add(ctx context.Context, args Args, reply *int) error {
	*reply = args.A + args.B
	return nil
}

func (t *Arith) Divide(ctx context.Context, args *Args, quo *Quotient) error {
	if args.B == 0 {
		return errors.New("divide by zero")
	}
	quo.Quo = args.A / args.B
	quo.Rem = args.A % args.B
	return nil
}

func (t *Arith) GimmeError(ctx context.Context, args *Args, r *int) error {
	*r = 42
	return errors.New("an error")
}

func (t *Arith) Sleep(ctx context.Context, secs int, res *struct{}) error {
	t.ctxTracker.setCtx(ctx)
	tim := time.NewTimer(time.Duration(secs) * time.Second)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-tim.C:
		return nil
	}
}

func makeRandomNodes() (h1, h2 host.Host) {
	priv1, pub1, _ := crypto.GenerateKeyPair(crypto.RSA, 2048)
	pid1, _ := peer.IDFromPublicKey(pub1)
	maddr1, _ := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/19998")

	priv2, pub2, _ := crypto.GenerateKeyPair(crypto.RSA, 2048)
	pid2, _ := peer.IDFromPublicKey(pub2)
	maddr2, _ := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/19999")

	ps1 := peerstore.NewPeerstore()
	ps2 := peerstore.NewPeerstore()
	ps1.AddPubKey(pid1, pub1)
	ps1.AddPrivKey(pid1, priv1)
	ps1.AddPubKey(pid2, pub2)
	ps1.AddPrivKey(pid2, priv2)
	ps1.AddAddrs(pid2, []multiaddr.Multiaddr{maddr2}, peerstore.PermanentAddrTTL)

	ps2.AddPubKey(pid1, pub1)
	ps2.AddPrivKey(pid1, priv1)
	ps2.AddPubKey(pid2, pub2)
	ps2.AddPrivKey(pid2, priv2)
	ps2.AddAddrs(pid1, []multiaddr.Multiaddr{maddr1}, peerstore.PermanentAddrTTL)

	ctx := context.Background()
	n1, _ := swarm.NewNetwork(
		ctx,
		[]multiaddr.Multiaddr{maddr1},
		pid1,
		ps1,
		nil)
	n2, _ := swarm.NewNetwork(
		ctx,
		[]multiaddr.Multiaddr{maddr2},
		pid2,
		ps2,
		nil)

	h1 = basic.New(n1)
	h2 = basic.New(n2)
	time.Sleep(time.Second)
	return
}

func TestRegister(t *testing.T) {
	h1, h2 := makeRandomNodes()
	defer h1.Close()
	defer h2.Close()
	s := NewServer(h1, "rpc")
	var arith Arith

	err := s.Register(arith)
	if err == nil {
		t.Error("expected an error")
	}
	err = s.Register(&arith)
	if err != nil {
		t.Error(err)
	}
	// Re-register
	err = s.Register(&arith)
	if err == nil {
		t.Error("expected an error")
	}

}

func testCall(t *testing.T, servNode, clientNode host.Host, dest peer.ID) {
	s := NewServer(servNode, "rpc")
	c := NewClientWithServer(clientNode, "rpc", s)
	var arith Arith
	s.Register(&arith)

	var r int
	err := c.Call("", "Arith", "Multiply", &Args{2, 3}, &r)
	if err != nil {
		t.Fatal(err)
	}
	if r != 6 {
		t.Error("result is:", r)
	}

	var a int
	err = c.Call("", "Arith", "Add", Args{2, 3}, &a)
	if err != nil {
		t.Fatal(err)
	}
	if a != 5 {
		t.Error("result is:", a)
	}

	var q Quotient
	err = c.Call(dest, "Arith", "Divide", &Args{20, 6}, &q)
	if err != nil {
		t.Fatal(err)
	}
	if q.Quo != 3 || q.Rem != 2 {
		t.Error("bad division")
	}
}

func TestCall(t *testing.T) {
	h1, h2 := makeRandomNodes()
	defer h1.Close()
	defer h2.Close()

	t.Run("local", func(t *testing.T) {
		testCall(t, h1, h2, "")
	})
	t.Run("remote", func(t *testing.T) {
		testCall(t, h1, h2, h1.ID())
	})
}

func TestErrorResponse(t *testing.T) {
	h1, h2 := makeRandomNodes()
	defer h1.Close()
	defer h2.Close()

	s := NewServer(h1, "rpc")
	var arith Arith
	s.Register(&arith)

	var r int
	// test remote
	c := NewClientWithServer(h2, "rpc", s)
	err := c.Call(h1.ID(), "Arith", "GimmeError", &Args{1, 2}, &r)
	if err == nil || err.Error() != "an error" {
		t.Error("expected different error")
	}
	if r != 42 {
		t.Error("response should be set even on error")
	}

	// test local
	c = NewClientWithServer(h1, "rpc", s)
	err = c.Call(h1.ID(), "Arith", "GimmeError", &Args{1, 2}, &r)
	if err == nil || err.Error() != "an error" {
		t.Error("expected different error")
	}
	if r != 42 {
		t.Error("response should be set even on error")
	}
}

func testCallContext(t *testing.T, servHost, clientHost host.Host, dest peer.ID) {
	s := NewServer(servHost, "rpc")
	c := NewClientWithServer(clientHost, "rpc", s)

	var arith Arith
	arith.ctxTracker = &ctxTracker{}
	s.Register(&arith)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second/2)
	defer cancel()
	err := c.CallContext(ctx, dest, "Arith", "Sleep", 5, &struct{}{})
	if err == nil {
		t.Fatal("expected an error")
	}

	if !strings.Contains(err.Error(), "context") {
		t.Error("expected a context error:", err)
	}

	time.Sleep(200 * time.Millisecond)

	if !arith.ctxTracker.cancelled() {
		t.Error("expected ctx cancellation in the function")
	}
}

func TestCallContext(t *testing.T) {
	h1, h2 := makeRandomNodes()
	defer h1.Close()
	defer h2.Close()

	t.Run("local", func(t *testing.T) {
		testCallContext(t, h1, h2, h2.ID())
	})

	t.Run("remote", func(t *testing.T) {
		testCallContext(t, h1, h2, h1.ID())
	})

	t.Run("async", func(t *testing.T) {
		s := NewServer(h1, "rpc")
		c := NewClientWithServer(h2, "rpc", s)

		var arith Arith
		arith.ctxTracker = &ctxTracker{}
		s.Register(&arith)

		ctx, cancel := context.WithTimeout(context.Background(), time.Second/2)
		defer cancel()

		done := make(chan *Call, 1)
		err := c.GoContext(ctx, h1.ID(), "Arith", "Sleep", 5, &struct{}{}, done)
		if err != nil {
			t.Fatal(err)
		}

		call := <-done
		if call.Error == nil || !strings.Contains(call.Error.Error(), "context") {
			t.Error("expected a context error:", err)
		}
	})
}

func TestMultiCall(t *testing.T) {
	h1, h2 := makeRandomNodes()
	defer h1.Close()
	defer h2.Close()

	s := NewServer(h1, "rpc")
	c := NewClientWithServer(h2, "rpc", s)
	var arith Arith
	s.Register(&arith)

	replies := make([]int, 2, 2)
	ctxs := make([]context.Context, 2, 2)
	repliesInt := make([]interface{}, 2, 2)
	for i := range repliesInt {
		repliesInt[i] = &replies[i]
		ctxs[i] = context.Background()
	}

	errs := c.MultiCall(
		ctxs,
		[]peer.ID{h1.ID(), h2.ID()},
		"Arith",
		"Multiply",
		&Args{2, 3},
		repliesInt,
	)

	if len(errs) != 2 {
		t.Fatal("expected two errs")
	}

	for _, err := range errs {
		if err != nil {
			t.Error(err)
		}
	}

	for _, reply := range replies {
		if reply != 6 {
			t.Error("expected 2*3=6")
		}
	}
}

func TestMultiGo(t *testing.T) {
	h1, h2 := makeRandomNodes()
	defer h1.Close()
	defer h2.Close()

	s := NewServer(h1, "rpc")
	c := NewClientWithServer(h2, "rpc", s)
	var arith Arith
	s.Register(&arith)

	replies := make([]int, 2, 2)
	ctxs := make([]context.Context, 2, 2)
	repliesInt := make([]interface{}, 2, 2)
	dones := make([]chan *Call, 2, 2)
	for i := range repliesInt {
		repliesInt[i] = &replies[i]
		ctxs[i] = context.Background()
		dones[i] = make(chan *Call, 1)
	}

	err := c.MultiGo(
		ctxs,
		[]peer.ID{h1.ID(), h2.ID()},
		"Arith",
		"Multiply",
		&Args{2, 3},
		repliesInt,
		dones,
	)

	if err != nil {
		t.Error(err)
	}

	<-dones[0]
	<-dones[1]

	for _, reply := range replies {
		if reply != 6 {
			t.Error("expected 2*3=6")
		}
	}
}
