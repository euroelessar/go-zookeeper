package zk

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestStateChanges(t *testing.T) {
	ts, err := StartTestCluster(1, nil, logWriter{t: t, p: "[ZKERR] "})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Stop()

	callbackChan := make(chan Event)
	f := func(event Event) {
		callbackChan <- event
	}

	zk, eventChan, err := ts.ConnectWithOptions(15*time.Second, WithEventCallback(f))
	if err != nil {
		t.Fatalf("Connect returned error: %+v", err)
	}

	verifyEventOrder := func(c <-chan Event, expectedStates []State, source string) {
		for _, state := range expectedStates {
			for {
				event, ok := <-c
				if !ok {
					t.Fatalf("unexpected channel close for %s", source)
				}

				if event.Type != EventSession {
					continue
				}

				if event.State != state {
					t.Fatalf("mismatched state order from %s, expected %v, received %v", source, state, event.State)
				}
				break
			}
		}
	}

	states := []State{StateConnecting, StateConnected, StateHasSession}
	verifyEventOrder(callbackChan, states, "callback")
	verifyEventOrder(eventChan, states, "event channel")

	zk.Close()
	verifyEventOrder(callbackChan, []State{StateDisconnected}, "callback")
	verifyEventOrder(eventChan, []State{StateDisconnected}, "event channel")
}

func TestCreate(t *testing.T) {
	ts, err := StartTestCluster(1, nil, logWriter{t: t, p: "[ZKERR] "})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Stop()
	zk, _, err := ts.ConnectAll()
	if err != nil {
		t.Fatalf("Connect returned error: %+v", err)
	}
	defer zk.Close()

	path := "/gozk-test"
	path2 := path + "2"

	if err := zk.Delete(path, -1); err != nil && err != ErrNoNode {
		t.Fatalf("Delete returned error: %+v", err)
	}

	if p, err := zk.Create(path, []byte{1, 2, 3, 4}, 0, WorldACL(PermAll)); err != nil {
		t.Fatalf("Create returned error: %+v", err)
	} else if p != path {
		t.Fatalf("Create returned different path '%s' != '%s'", p, path)
	}

	// Zookeeper 3.5.x releases support a more fully featured create2
	// operation that is not backward compatible. Assume any test
	// cluster is homogenous and check only the first instance.
	srvrStats, ok := FLWSrvr([]string{fmt.Sprintf("localhost:%v", ts.Servers[0].Port)}, 500*time.Millisecond)
	if !ok {
		t.Fatalf("FLWSrvr failed error: %v", srvrStats[0].Error)
	}

	if strings.HasPrefix(srvrStats[0].Version, "3.5.") {
		if p, stat, err := zk.Create2(path2, []byte{1, 2, 3, 4}, 0, WorldACL(PermAll)); err != nil {
			t.Fatalf("Create returned error: %+v", err)
		} else if p != path2 {
			t.Fatalf("Create returned different path '%s' != '%s'", p, path2)
		} else if stat.Czxid == 0 || stat.Mzxid == 0 {
			t.Fatalf("Create returned invalid stat czxid:0x%X mzxid:0x%X", stat.Czxid, stat.Mzxid)
		}
		if p, stat, err := zk.Create2(path2, nil, 0, WorldACL(PermAll)); err == nil {
			t.Fatalf("Create should have failed with NodeExists")
		} else if stat != nil {
			t.Fatalf("Create should have returned nil stat on failure.")
		} else if p != "" {
			t.Fatalf("Create should have returned empty path on failure.")
		}
	}
	if data, stat, err := zk.Get(path); err != nil {
		t.Fatalf("Get returned error: %+v", err)
	} else if stat == nil {
		t.Fatal("Get returned nil stat")
	} else if len(data) < 4 {
		t.Fatal("Get returned wrong size data")
	}

}

func TestMulti(t *testing.T) {
	ts, err := StartTestCluster(1, nil, logWriter{t: t, p: "[ZKERR] "})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Stop()
	zk, _, err := ts.ConnectAll()
	if err != nil {
		t.Fatalf("Connect returned error: %+v", err)
	}
	defer zk.Close()

	path := "/gozk-test"

	if err := zk.Delete(path, -1); err != nil && err != ErrNoNode {
		t.Fatalf("Delete returned error: %+v", err)
	}
	ops := []interface{}{
		&CreateRequest{Path: path, Data: []byte{1, 2, 3, 4}, Acl: WorldACL(PermAll)},
		&SetDataRequest{Path: path, Data: []byte{1, 2, 3, 4}, Version: -1},
	}
	if res, err := zk.Multi(ops...); err != nil {
		t.Fatalf("Multi returned error: %+v", err)
	} else if len(res) != 2 {
		t.Fatalf("Expected 2 responses got %d", len(res))
	} else {
		t.Logf("%+v", res)
	}
	if data, stat, err := zk.Get(path); err != nil {
		t.Fatalf("Get returned error: %+v", err)
	} else if stat == nil {
		t.Fatal("Get returned nil stat")
	} else if len(data) < 4 {
		t.Fatal("Get returned wrong size data")
	}
}

func TestIfAuthdataSurvivesReconnect(t *testing.T) {
	// This test case ensures authentication data is being resubmited after
	// reconnect.
	testNode := "/auth-testnode"

	ts, err := StartTestCluster(1, nil, logWriter{t: t, p: "[ZKERR] "})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Stop()

	zk, _, err := ts.ConnectAll()
	if err != nil {
		t.Fatalf("Connect returned error: %+v", err)
	}
	defer zk.Close()

	acl := DigestACL(PermAll, "userfoo", "passbar")

	_, err = zk.Create(testNode, []byte("Some very secret content"), 0, acl)
	if err != nil && err != ErrNodeExists {
		t.Fatalf("Failed to create test node : %+v", err)
	}

	_, _, err = zk.Get(testNode)
	if err == nil || err != ErrNoAuth {
		var msg string

		if err == nil {
			msg = "Fetching data without auth should have resulted in an error"
		} else {
			msg = fmt.Sprintf("Expecting ErrNoAuth, got `%+v` instead", err)
		}
		t.Fatalf(msg)
	}

	zk.AddAuth("digest", []byte("userfoo:passbar"))

	_, _, err = zk.Get(testNode)
	if err != nil {
		t.Fatalf("Fetching data with auth failed: %+v", err)
	}

	ts.StopAllServers()
	ts.StartAllServers()

	_, _, err = zk.Get(testNode)
	if err != nil {
		t.Fatalf("Fetching data after reconnect failed: %+v", err)
	}
}

func TestMultiFailures(t *testing.T) {
	// This test case ensures that we return the errors associated with each
	// opeThis in the event a call to Multi() fails.
	const firstPath = "/gozk-test-first"
	const secondPath = "/gozk-test-second"

	ts, err := StartTestCluster(1, nil, logWriter{t: t, p: "[ZKERR] "})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Stop()
	zk, _, err := ts.ConnectAll()
	if err != nil {
		t.Fatalf("Connect returned error: %+v", err)
	}
	defer zk.Close()

	// Ensure firstPath doesn't exist and secondPath does. This will cause the
	// 2nd operation in the Multi() to fail.
	if err := zk.Delete(firstPath, -1); err != nil && err != ErrNoNode {
		t.Fatalf("Delete returned error: %+v", err)
	}
	if _, err := zk.Create(secondPath, nil /* data */, 0, WorldACL(PermAll)); err != nil {
		t.Fatalf("Create returned error: %+v", err)
	}

	ops := []interface{}{
		&CreateRequest{Path: firstPath, Data: []byte{1, 2}, Acl: WorldACL(PermAll)},
		&CreateRequest{Path: secondPath, Data: []byte{3, 4}, Acl: WorldACL(PermAll)},
	}
	res, err := zk.Multi(ops...)
	if err != ErrNodeExists {
		t.Fatalf("Multi() didn't return correct error: %+v", err)
	}
	if len(res) != 2 {
		t.Fatalf("Expected 2 responses received %d", len(res))
	}
	if res[0].Error != nil {
		t.Fatalf("First operation returned an unexpected error %+v", res[0].Error)
	}
	if res[1].Error != ErrNodeExists {
		t.Fatalf("Second operation returned incorrect error %+v", res[1].Error)
	}
	if _, _, err := zk.Get(firstPath); err != ErrNoNode {
		t.Fatalf("Node %s was incorrectly created: %+v", firstPath, err)
	}
}

func TestGetSetACL(t *testing.T) {
	ts, err := StartTestCluster(1, nil, logWriter{t: t, p: "[ZKERR] "})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Stop()
	zk, _, err := ts.ConnectAll()
	if err != nil {
		t.Fatalf("Connect returned error: %+v", err)
	}
	defer zk.Close()

	if err := zk.AddAuth("digest", []byte("blah")); err != nil {
		t.Fatalf("AddAuth returned error %+v", err)
	}

	path := "/gozk-test"

	if err := zk.Delete(path, -1); err != nil && err != ErrNoNode {
		t.Fatalf("Delete returned error: %+v", err)
	}
	if path, err := zk.Create(path, []byte{1, 2, 3, 4}, 0, WorldACL(PermAll)); err != nil {
		t.Fatalf("Create returned error: %+v", err)
	} else if path != "/gozk-test" {
		t.Fatalf("Create returned different path '%s' != '/gozk-test'", path)
	}

	expected := WorldACL(PermAll)

	if acl, stat, err := zk.GetACL(path); err != nil {
		t.Fatalf("GetACL returned error %+v", err)
	} else if stat == nil {
		t.Fatalf("GetACL returned nil Stat")
	} else if len(acl) != 1 || expected[0] != acl[0] {
		t.Fatalf("GetACL mismatch expected %+v instead of %+v", expected, acl)
	}

	expected = []ACL{{PermAll, "ip", "127.0.0.1"}}

	if stat, err := zk.SetACL(path, expected, -1); err != nil {
		t.Fatalf("SetACL returned error %+v", err)
	} else if stat == nil {
		t.Fatalf("SetACL returned nil Stat")
	}

	if acl, stat, err := zk.GetACL(path); err != nil {
		t.Fatalf("GetACL returned error %+v", err)
	} else if stat == nil {
		t.Fatalf("GetACL returned nil Stat")
	} else if len(acl) != 1 || expected[0] != acl[0] {
		t.Fatalf("GetACL mismatch expected %+v instead of %+v", expected, acl)
	}
}

func TestAuth(t *testing.T) {
	ts, err := StartTestCluster(1, nil, logWriter{t: t, p: "[ZKERR] "})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Stop()
	zk, _, err := ts.ConnectAll()
	if err != nil {
		t.Fatalf("Connect returned error: %+v", err)
	}
	defer zk.Close()

	path := "/gozk-digest-test"
	if err := zk.Delete(path, -1); err != nil && err != ErrNoNode {
		t.Fatalf("Delete returned error: %+v", err)
	}

	acl := DigestACL(PermAll, "user", "password")

	if p, err := zk.Create(path, []byte{1, 2, 3, 4}, 0, acl); err != nil {
		t.Fatalf("Create returned error: %+v", err)
	} else if p != path {
		t.Fatalf("Create returned different path '%s' != '%s'", p, path)
	}

	if a, stat, err := zk.GetACL(path); err != nil {
		t.Fatalf("GetACL returned error %+v", err)
	} else if stat == nil {
		t.Fatalf("GetACL returned nil Stat")
	} else if len(a) != 1 || acl[0] != a[0] {
		t.Fatalf("GetACL mismatch expected %+v instead of %+v", acl, a)
	}

	if _, _, err := zk.Get(path); err != ErrNoAuth {
		t.Fatalf("Get returned error %+v instead of ErrNoAuth", err)
	}

	if err := zk.AddAuth("digest", []byte("user:password")); err != nil {
		t.Fatalf("AddAuth returned error %+v", err)
	}

	if data, stat, err := zk.Get(path); err != nil {
		t.Fatalf("Get returned error %+v", err)
	} else if stat == nil {
		t.Fatalf("Get returned nil Stat")
	} else if len(data) != 4 {
		t.Fatalf("Get returned wrong data length")
	}
}

func TestChildren(t *testing.T) {
	ts, err := StartTestCluster(1, nil, logWriter{t: t, p: "[ZKERR] "})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Stop()
	zk, _, err := ts.ConnectAll()
	if err != nil {
		t.Fatalf("Connect returned error: %+v", err)
	}
	defer zk.Close()

	deleteNode := func(node string) {
		if err := zk.Delete(node, -1); err != nil && err != ErrNoNode {
			t.Fatalf("Delete returned error: %+v", err)
		}
	}

	deleteNode("/gozk-test-big")

	if path, err := zk.Create("/gozk-test-big", []byte{1, 2, 3, 4}, 0, WorldACL(PermAll)); err != nil {
		t.Fatalf("Create returned error: %+v", err)
	} else if path != "/gozk-test-big" {
		t.Fatalf("Create returned different path '%s' != '/gozk-test-big'", path)
	}

	rb := make([]byte, 1000)
	hb := make([]byte, 2000)
	prefix := []byte("/gozk-test-big/")
	for i := 0; i < 10000; i++ {
		_, err := rand.Read(rb)
		if err != nil {
			t.Fatal("Cannot create random znode name")
		}
		hex.Encode(hb, rb)

		expect := string(append(prefix, hb...))
		if path, err := zk.Create(expect, []byte{1, 2, 3, 4}, 0, WorldACL(PermAll)); err != nil {
			t.Fatalf("Create returned error: %+v", err)
		} else if path != expect {
			t.Fatalf("Create returned different path '%s' != '%s'", path, expect)
		}
		defer deleteNode(string(expect))
	}

	children, _, err := zk.Children("/gozk-test-big")
	if err != nil {
		t.Fatalf("Children returned error: %+v", err)
	} else if len(children) != 10000 {
		t.Fatal("Children returned wrong number of nodes")
	}
}

func TestChildWatch(t *testing.T) {
	ts, err := StartTestCluster(1, nil, logWriter{t: t, p: "[ZKERR] "})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Stop()
	zk, _, err := ts.ConnectAll()
	if err != nil {
		t.Fatalf("Connect returned error: %+v", err)
	}
	defer zk.Close()

	if err := zk.Delete("/gozk-test", -1); err != nil && err != ErrNoNode {
		t.Fatalf("Delete returned error: %+v", err)
	}

	children, stat, childCh, err := zk.ChildrenW("/")
	if err != nil {
		t.Fatalf("Children returned error: %+v", err)
	} else if stat == nil {
		t.Fatal("Children returned nil stat")
	} else if len(children) < 1 {
		t.Fatal("Children should return at least 1 child")
	}

	if path, err := zk.Create("/gozk-test", []byte{1, 2, 3, 4}, 0, WorldACL(PermAll)); err != nil {
		t.Fatalf("Create returned error: %+v", err)
	} else if path != "/gozk-test" {
		t.Fatalf("Create returned different path '%s' != '/gozk-test'", path)
	}

	select {
	case ev := <-childCh:
		if ev.Err != nil {
			t.Fatalf("Child watcher error %+v", ev.Err)
		}
		if ev.Path != "/" {
			t.Fatalf("Child watcher wrong path %s instead of %s", ev.Path, "/")
		}
	case _ = <-time.After(time.Second * 2):
		t.Fatal("Child watcher timed out")
	}

	// Delete of the watched node should trigger the watch

	children, stat, childCh, err = zk.ChildrenW("/gozk-test")
	if err != nil {
		t.Fatalf("Children returned error: %+v", err)
	} else if stat == nil {
		t.Fatal("Children returned nil stat")
	} else if len(children) != 0 {
		t.Fatal("Children should return 0 children")
	}

	if err := zk.Delete("/gozk-test", -1); err != nil && err != ErrNoNode {
		t.Fatalf("Delete returned error: %+v", err)
	}

	select {
	case ev := <-childCh:
		if ev.Err != nil {
			t.Fatalf("Child watcher error %+v", ev.Err)
		}
		if ev.Path != "/gozk-test" {
			t.Fatalf("Child watcher wrong path %s instead of %s", ev.Path, "/")
		}
	case _ = <-time.After(time.Second * 2):
		t.Fatal("Child watcher timed out")
	}
}

func TestSetWatchers(t *testing.T) {
	ts, err := StartTestCluster(1, nil, logWriter{t: t, p: "[ZKERR] "})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Stop()
	zk, _, err := ts.ConnectAll()
	if err != nil {
		t.Fatalf("Connect returned error: %+v", err)
	}
	defer zk.Close()

	zk.reconnectDelay = time.Second

	zk2, _, err := ts.ConnectAll()
	if err != nil {
		t.Fatalf("Connect returned error: %+v", err)
	}
	defer zk2.Close()

	if err := zk.Delete("/gozk-test", -1); err != nil && err != ErrNoNode {
		t.Fatalf("Delete returned error: %+v", err)
	}

	testPath, err := zk.Create("/gozk-test-2", []byte{}, 0, WorldACL(PermAll))
	if err != nil {
		t.Fatalf("Create returned: %+v", err)
	}

	_, _, testEvCh, err := zk.GetW(testPath)
	if err != nil {
		t.Fatalf("GetW returned: %+v", err)
	}

	children, stat, childCh, err := zk.ChildrenW("/")
	if err != nil {
		t.Fatalf("Children returned error: %+v", err)
	} else if stat == nil {
		t.Fatal("Children returned nil stat")
	} else if len(children) < 1 {
		t.Fatal("Children should return at least 1 child")
	}

	// Simulate network error by brutally closing the network connection.
	zk.conn.Close()
	if err := zk2.Delete(testPath, -1); err != nil && err != ErrNoNode {
		t.Fatalf("Delete returned error: %+v", err)
	}
	// Allow some time for the `zk` session to reconnect and set watches.
	time.Sleep(time.Millisecond * 100)

	if path, err := zk2.Create("/gozk-test", []byte{1, 2, 3, 4}, 0, WorldACL(PermAll)); err != nil {
		t.Fatalf("Create returned error: %+v", err)
	} else if path != "/gozk-test" {
		t.Fatalf("Create returned different path '%s' != '/gozk-test'", path)
	}

	select {
	case ev := <-testEvCh:
		if ev.Err != nil {
			t.Fatalf("GetW watcher error %+v", ev.Err)
		}
		if ev.Path != testPath {
			t.Fatalf("GetW watcher wrong path %s instead of %s", ev.Path, testPath)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GetW watcher timed out")
	}

	select {
	case ev := <-childCh:
		if ev.Err != nil {
			t.Fatalf("Child watcher error %+v", ev.Err)
		}
		if ev.Path != "/" {
			t.Fatalf("Child watcher wrong path %s instead of %s", ev.Path, "/")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Child watcher timed out")
	}
}

func TestExpiringWatch(t *testing.T) {
	ts, err := StartTestCluster(1, nil, logWriter{t: t, p: "[ZKERR] "})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Stop()
	zk, _, err := ts.ConnectAll()
	if err != nil {
		t.Fatalf("Connect returned error: %+v", err)
	}
	defer zk.Close()

	if err := zk.Delete("/gozk-test", -1); err != nil && err != ErrNoNode {
		t.Fatalf("Delete returned error: %+v", err)
	}

	children, stat, childCh, err := zk.ChildrenW("/")
	if err != nil {
		t.Fatalf("Children returned error: %+v", err)
	} else if stat == nil {
		t.Fatal("Children returned nil stat")
	} else if len(children) < 1 {
		t.Fatal("Children should return at least 1 child")
	}

	zk.sessionID = 99999
	zk.conn.Close()

	select {
	case ev := <-childCh:
		if ev.Err != ErrSessionExpired {
			t.Fatalf("Child watcher error %+v instead of expected ErrSessionExpired", ev.Err)
		}
		if ev.Path != "/" {
			t.Fatalf("Child watcher wrong path %s instead of %s", ev.Path, "/")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Child watcher timed out")
	}
}

func TestRequestFail(t *testing.T) {
	// If connecting fails to all servers in the list then pending requests
	// should be errored out so they don't hang forever.

	zk, _, err := Connect([]string{"127.0.0.1:32444"}, time.Second*15)
	if err != nil {
		t.Fatal(err)
	}
	defer zk.Close()

	ch := make(chan error)
	go func() {
		_, _, err := zk.Get("/blah")
		ch <- err
	}()
	select {
	case err := <-ch:
		if err == nil {
			t.Fatal("Expected non-nil error on failed request due to connection failure")
		}
	case <-time.After(time.Second * 2):
		t.Fatal("Get hung when connection could not be made")
	}
}

func TestSlowServer(t *testing.T) {
	ts, err := StartTestCluster(1, nil, logWriter{t: t, p: "[ZKERR] "})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Stop()

	realAddr := fmt.Sprintf("127.0.0.1:%d", ts.Servers[0].Port)
	proxyAddr, stopCh, err := startSlowProxy(t,
		Rate{}, Rate{},
		realAddr, func(ln *Listener) {
			if ln.Up.Latency == 0 {
				ln.Up.Latency = time.Millisecond * 2000
				ln.Down.Latency = time.Millisecond * 2000
			} else {
				ln.Up.Latency = 0
				ln.Down.Latency = 0
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	defer close(stopCh)

	zk, _, err := Connect([]string{proxyAddr}, time.Millisecond*500)
	if err != nil {
		t.Fatal(err)
	}
	defer zk.Close()

	_, _, wch, err := zk.ChildrenW("/")
	if err != nil {
		t.Fatal(err)
	}

	// Force a reconnect to get a throttled connection
	zk.conn.Close()

	time.Sleep(time.Millisecond * 100)

	if err := zk.Delete("/gozk-test", -1); err == nil {
		t.Fatal("Delete should have failed")
	}

	// The previous request should have timed out causing the server to be disconnected and reconnected

	if _, err := zk.Create("/gozk-test", []byte{1, 2, 3, 4}, 0, WorldACL(PermAll)); err != nil {
		t.Fatal(err)
	}

	// Make sure event is still returned because the session should not have been affected
	select {
	case ev := <-wch:
		t.Logf("Received event: %+v", ev)
	case <-time.After(time.Second):
		t.Fatal("Expected to receive a watch event")
	}
}

func startSlowProxy(t *testing.T, up, down Rate, upstream string, adj func(ln *Listener)) (string, chan bool, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	tln := &Listener{
		Listener: ln,
		Up:       up,
		Down:     down,
	}
	stopCh := make(chan bool)
	go func() {
		<-stopCh
		tln.Close()
	}()
	go func() {
		for {
			cn, err := tln.Accept()
			if err != nil {
				if !strings.Contains(err.Error(), "use of closed network connection") {
					t.Fatalf("Accept failed: %s", err.Error())
				}
				return
			}
			if adj != nil {
				adj(tln)
			}
			go func(cn net.Conn) {
				defer cn.Close()
				upcn, err := net.Dial("tcp", upstream)
				if err != nil {
					t.Log(err)
					return
				}
				// This will leave hanging goroutines util stopCh is closed
				// but it doesn't matter in the context of running tests.
				go func() {
					<-stopCh
					upcn.Close()
				}()
				go func() {
					if _, err := io.Copy(upcn, cn); err != nil {
						if !strings.Contains(err.Error(), "use of closed network connection") {
							// log.Printf("Upstream write failed: %s", err.Error())
						}
					}
				}()
				if _, err := io.Copy(cn, upcn); err != nil {
					if !strings.Contains(err.Error(), "use of closed network connection") {
						// log.Printf("Upstream read failed: %s", err.Error())
					}
				}
			}(cn)
		}
	}()
	return ln.Addr().String(), stopCh, nil
}
