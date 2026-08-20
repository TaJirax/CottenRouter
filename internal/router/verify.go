package router

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"strings"

	"github.com/TaJirax/CottenRouter/internal/dnswire"
)

var verificationBase32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// verificationResponse implements SlipGate/SlipNet's authenticated reachability
// and MTU probe. Invalid proofs are indistinguishable from ordinary tunnel
// traffic and continue to the configured backend.
func (s *Server) verificationResponse(packet []byte, route routeEntry) ([]byte, bool) {
	if len(route.verifyKey) == 0 || binary.BigEndian.Uint16(packet[4:6]) != 1 {
		return nil, false
	}
	question, err := dnswire.ParseQuestion(packet)
	if err != nil || question.Type != 16 || question.Class != 1 {
		return nil, false
	}
	suffix := "." + route.domain
	if !strings.HasSuffix(question.Name, suffix) {
		return nil, false
	}
	prefix := strings.TrimSuffix(question.Name, suffix)
	encoded := strings.ReplaceAll(prefix, ".", "")
	decoded, err := verificationBase32.DecodeString(encoded)
	if err != nil || len(decoded) < 32 {
		return nil, false
	}
	nonce, proof := decoded[:16], decoded[16:32]
	requestMAC := hmac.New(sha256.New, route.verifyKey)
	requestMAC.Write(nonce)
	if !hmac.Equal(proof, requestMAC.Sum(nil)[:16]) {
		return nil, false
	}

	responseMAC := hmac.New(sha256.New, route.verifyKey)
	responseMAC.Write(nonce)
	responseMAC.Write([]byte{1})
	payload := responseMAC.Sum(nil)

	target := route.verifyMTU
	if target == 0 {
		target = 1232
	}
	if requested := int(binary.BigEndian.Uint16(nonce[14:16])); requested >= 200 && requested <= 4096 {
		target = requested
	} else if advertised := ednsPayloadSize(packet, question.End); advertised >= 200 && advertised <= 4096 {
		target = advertised
	}
	if target > s.cfg.MaxPacketSize {
		target = s.cfg.MaxPacketSize
	}
	dataSize := target - question.End - 25
	dataSize -= dataSize/256 + 1
	if dataSize < len(payload) {
		dataSize = len(payload)
	}
	padded := make([]byte, dataSize)
	copy(padded, payload)
	if len(padded) > len(payload) {
		_, _ = rand.Read(padded[len(payload):])
	}
	return binaryTXTResponse(packet, question.End, padded), true
}

func binaryTXTResponse(query []byte, questionEnd int, data []byte) []byte {
	response := make([]byte, 0, questionEnd+len(data)+32)
	response = append(response, query[0], query[1], 0x84, 0x00)
	response = append(response, 0, 1, 0, 1, 0, 0, 0, 1)
	response = append(response, query[12:questionEnd]...)
	response = append(response, 0xc0, 0x0c, 0, 16, 0, 1, 0, 0, 0, 60)
	rdataLengthIndex := len(response)
	response = append(response, 0, 0)
	rdataStart := len(response)
	for len(data) > 0 {
		size := len(data)
		if size > 255 {
			size = 255
		}
		response = append(response, byte(size))
		response = append(response, data[:size]...)
		data = data[size:]
	}
	binary.BigEndian.PutUint16(response[rdataLengthIndex:rdataLengthIndex+2], uint16(len(response)-rdataStart))
	response = append(response, 0, 0, 41, 0x10, 0, 0, 0, 0, 0, 0, 0)
	return response
}

func ednsPayloadSize(packet []byte, offset int) int {
	counts := int(binary.BigEndian.Uint16(packet[6:8])) + int(binary.BigEndian.Uint16(packet[8:10]))
	for i := 0; i < counts; i++ {
		offset = skipDNSName(packet, offset)
		if offset < 0 || offset+10 > len(packet) {
			return 0
		}
		rdataLength := int(binary.BigEndian.Uint16(packet[offset+8 : offset+10]))
		offset += 10 + rdataLength
	}
	additional := int(binary.BigEndian.Uint16(packet[10:12]))
	for i := 0; i < additional; i++ {
		offset = skipDNSName(packet, offset)
		if offset < 0 || offset+10 > len(packet) {
			return 0
		}
		if binary.BigEndian.Uint16(packet[offset:offset+2]) == 41 {
			return int(binary.BigEndian.Uint16(packet[offset+2 : offset+4]))
		}
		rdataLength := int(binary.BigEndian.Uint16(packet[offset+8 : offset+10]))
		offset += 10 + rdataLength
	}
	return 0
}

func skipDNSName(packet []byte, offset int) int {
	for offset < len(packet) {
		length := int(packet[offset])
		if length == 0 {
			return offset + 1
		}
		if length&0xc0 == 0xc0 {
			if offset+1 >= len(packet) {
				return -1
			}
			return offset + 2
		}
		if length > 63 || offset+1+length > len(packet) {
			return -1
		}
		offset += 1 + length
	}
	return -1
}
