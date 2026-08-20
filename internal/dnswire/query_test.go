package dnswire

import (
	"encoding/binary"
	"errors"
	"testing"
)

func query(name string, id uint16) []byte {
	packet := make([]byte, 12)
	binary.BigEndian.PutUint16(packet[:2], id)
	binary.BigEndian.PutUint16(packet[2:4], 0x0100)
	binary.BigEndian.PutUint16(packet[4:6], 1)
	start := 0
	for i := 0; i <= len(name); i++ {
		if i == len(name) || name[i] == '.' {
			packet = append(packet, byte(i-start))
			packet = append(packet, name[start:i]...)
			start = i + 1
		}
	}
	packet = append(packet, 0, 0, 1, 0, 1)
	return packet
}

func TestQuestionName(t *testing.T) {
	got, err := QuestionName(query("X.VPN.Example.COM", 7))
	if err != nil {
		t.Fatal(err)
	}
	if got != "x.vpn.example.com" {
		t.Fatalf("got %q", got)
	}
}

func TestQuestionNameRejectsCompression(t *testing.T) {
	packet := query("x.example", 1)
	packet[12] = 0xc0
	if _, err := QuestionName(packet); !errors.Is(err, ErrCompressedQNAME) {
		t.Fatalf("got %v", err)
	}
}

func TestRefusedResponse(t *testing.T) {
	packet := query("unknown.example", 0xbeef)
	response := RefusedResponse(packet)
	if binary.BigEndian.Uint16(response[:2]) != 0xbeef {
		t.Fatal("transaction ID changed")
	}
	flags := binary.BigEndian.Uint16(response[2:4])
	if flags&0x8000 == 0 || flags&0xf != 5 {
		t.Fatalf("unexpected flags %#x", flags)
	}
}
