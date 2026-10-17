package main

import (
	"reflect"
	"testing"

	"github.com/RooseveltAdvisors/vimline/keys"
)

// The burst bug: ggt in one read used to yield g and drop gt. Split keeps all.
func TestBurstKeysReachTheDispatcher(t *testing.T) {
	d := keys.New()
	d.Register("t", map[string]string{"c": "tab-create"}, "c create")
	var got []string
	for _, k := range keys.Split([]byte("tc")) {
		if a, r := d.Feed(k); r == keys.Chord {
			got = append(got, a.Name)
		}
	}
	if !reflect.DeepEqual(got, []string{"tab-create"}) {
		t.Fatalf("chords from burst = %v", got)
	}
}
