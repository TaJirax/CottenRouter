package router

import (
	"testing"

	"github.com/TaJirax/CottenRouter/internal/config"
)

func TestRouteTableUsesLongestSuffix(t *testing.T) {
	table, err := newRouteTable([]config.Route{
		{Name: "parent", Domains: []string{"example.com"}, Backend: "127.0.0.1:1"},
		{Name: "specific", Domains: []string{"vpn.example.com"}, Backend: "127.0.0.1:2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := table.match("payload.vpn.example.com")
	if !ok || got.name != "specific" {
		t.Fatalf("got %+v, %v", got, ok)
	}
	if _, ok := table.match("notexample.com"); ok {
		t.Fatal("matched a non-label suffix")
	}
}
