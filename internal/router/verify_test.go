package router

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"io"
	"log/slog"
	"testing"

	"github.com/TaJirax/CottenRouter/internal/config"
	"github.com/TaJirax/CottenRouter/internal/dnswire"
)

func TestSlipGateVerificationProbe(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	nonce := []byte("verification0000")
	binary.BigEndian.PutUint16(nonce[14:16], 300)
	mac := hmac.New(sha256.New, key)
	mac.Write(nonce)
	probe := append(append([]byte(nil), nonce...), mac.Sum(nil)[:16]...)
	encoded := base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding).EncodeToString(probe)
	packet := makeQuery(encoded+".slip.example", 0x7788)

	cfg := config.Config{Routes: []config.Route{{
		Name: "slipgate:test", Domains: []string{"slip.example"}, Backend: "127.0.0.1:5310", TCPBackend: "disabled",
		Verify: &config.VerifyConfig{Key: hex.EncodeToString(key), MTU: 1232},
	}}}
	server, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	response, ok := server.verificationResponse(packet, server.routes[0])
	if !ok {
		t.Fatal("valid verification probe was not recognized")
	}
	if len(response) < 200 || len(response) > 300 {
		t.Fatalf("response length %d does not honor requested MTU", len(response))
	}
	question, err := dnswire.ParseQuestion(packet)
	if err != nil {
		t.Fatal(err)
	}
	rdata := question.End + 12
	if rdata+33 > len(response) {
		t.Fatal("verification TXT answer is too short")
	}
	wantMAC := hmac.New(sha256.New, key)
	wantMAC.Write(nonce)
	wantMAC.Write([]byte{1})
	if !hmac.Equal(response[rdata+1:rdata+33], wantMAC.Sum(nil)) {
		t.Fatal("verification response HMAC is wrong")
	}

	packet[13] ^= 1
	if _, ok := server.verificationResponse(packet, server.routes[0]); ok {
		t.Fatal("invalid proof was accepted")
	}
}
