package main

import (
	"bytes"
	"testing"
)

func TestPackRoundTrip(t *testing.T) {
	objs := []*Obj{
		{Type: "blob", Data: []byte("hello world\n")},
		{Type: "blob", Data: bytes.Repeat([]byte("x"), 5000)},
		{Type: "tree", Data: []byte("100644 f\x00" + string([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}))},
		{Type: "commit", Data: []byte("tree 0000000000000000000000000000000000000000\n\nmsg\n")},
	}
	pack := writePack(objs)
	back, err := readPack(pack, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != len(objs) {
		t.Fatalf("got %d objects, want %d", len(back), len(objs))
	}
	for i, o := range back {
		if o.Type != objs[i].Type || !bytes.Equal(o.Data, objs[i].Data) {
			t.Fatalf("object %d mismatch: %+v vs %+v", i, o, objs[i])
		}
	}
	// second round-trip reuses pooled readers/writers
	pack2 := writePack(objs)
	back2, err := readPack(pack2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(back2) != len(objs) {
		t.Fatalf("round 2: got %d objects", len(back2))
	}
	if !bytes.Equal(pack, pack2) {
		t.Fatal("pack not deterministic")
	}
}
