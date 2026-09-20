package registry

import (
	"net"
	"sync"
	"testing"

	"github.com/hashicorp/memberlist"
)

func TestDirectory_SetKeepsAPrivateCopy(t *testing.T) {
	d := newDirectory()
	n := &memberlist.Node{Name: "peer", Addr: net.ParseIP("10.0.0.1"), Port: 7946, Meta: []byte(`{"id":"peer"}`)}
	d.set(n)

	// memberlist rewrites the node it handed over once the callback returns.
	copy(n.Meta, `XXXXXXXXXXXXX`)
	n.Addr[len(n.Addr)-1] = 99
	n.Port = 1

	got, ok := d.get("peer")
	if !ok {
		t.Fatal("peer is missing from the directory")
	}
	if string(got.Meta) != `{"id":"peer"}` {
		t.Errorf("Meta = %q; the directory kept memberlist's slice", got.Meta)
	}
	if !got.Addr.Equal(net.ParseIP("10.0.0.1")) || got.Port != 7946 {
		t.Errorf("address = %v:%d; the directory kept memberlist's node", got.Addr, got.Port)
	}
}

func TestDirectory_SetReplacesAndRemoveForgets(t *testing.T) {
	d := newDirectory()
	d.set(&memberlist.Node{Name: "a", Meta: []byte("one")})
	d.set(&memberlist.Node{Name: "b", Meta: []byte("b")})
	d.set(&memberlist.Node{Name: "a", Meta: []byte("two")})

	if got, _ := d.get("a"); string(got.Meta) != "two" {
		t.Errorf("a's Meta = %q, want the later one", got.Meta)
	}
	if n := len(d.all()); n != 2 {
		t.Errorf("all returned %d members, want 2", n)
	}

	d.remove("a")
	if _, ok := d.get("a"); ok {
		t.Error("a is still in the directory after remove")
	}
	d.remove("never-there")
	if n := len(d.all()); n != 1 {
		t.Errorf("all returned %d members, want 1", n)
	}
}

func TestDirectory_AllHandsOutDistinctCopies(t *testing.T) {
	d := newDirectory()
	d.set(&memberlist.Node{Name: "a"})
	d.set(&memberlist.Node{Name: "b"})

	all := d.all()
	if len(all) != 2 || all[0] == all[1] || all[0].Name == all[1].Name {
		t.Fatalf("all = %v; want two distinct members", all)
	}
	all[0].Name = "changed-by-reader"
	if _, ok := d.get("changed-by-reader"); ok {
		t.Error("a reader renamed a member inside the directory")
	}
}

func TestDirectory_ConcurrentUse(t *testing.T) {
	d := newDirectory()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 500 {
				d.set(&memberlist.Node{Name: "peer", Meta: []byte("m")})
				d.remove("peer")
			}
		}()
		go func() {
			defer wg.Done()
			for range 500 {
				_, _ = d.get("peer")
				_ = d.all()
			}
		}()
	}
	wg.Wait()
}
