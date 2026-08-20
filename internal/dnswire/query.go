package dnswire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrShortPacket      = errors.New("DNS packet is shorter than its header")
	ErrNotQuery         = errors.New("DNS packet is not a query")
	ErrNoQuestion       = errors.New("DNS packet has no question")
	ErrCompressedQNAME  = errors.New("compressed QNAME is not accepted in a query")
	ErrInvalidLabel     = errors.New("invalid DNS label")
	ErrUnterminatedName = errors.New("unterminated DNS name")
)

type Question struct {
	Name  string
	Type  uint16
	Class uint16
	End   int
}

// QuestionName returns the normalized first QNAME from a standard DNS query.
// Tunnel clients put their routing suffix in the first question. Compression
// pointers are deliberately rejected here: they are unnecessary in a query's
// first QNAME and accepting them would make bounds checking more complicated.
func QuestionName(packet []byte) (string, error) {
	question, err := ParseQuestion(packet)
	return question.Name, err
}

// ParseQuestion returns the first DNS question and the byte offset immediately
// after its QCLASS field.
func ParseQuestion(packet []byte) (Question, error) {
	if len(packet) < 12 {
		return Question{}, ErrShortPacket
	}
	if binary.BigEndian.Uint16(packet[2:4])&0x8000 != 0 {
		return Question{}, ErrNotQuery
	}
	if binary.BigEndian.Uint16(packet[4:6]) == 0 {
		return Question{}, ErrNoQuestion
	}

	labels := make([]string, 0, 8)
	for offset := 12; ; {
		if offset >= len(packet) {
			return Question{}, ErrUnterminatedName
		}
		length := int(packet[offset])
		offset++
		if length == 0 {
			if len(labels) == 0 {
				return Question{}, ErrInvalidLabel
			}
			if offset+4 > len(packet) {
				return Question{}, ErrShortPacket
			}
			return Question{
				Name:  strings.ToLower(strings.Join(labels, ".")),
				Type:  binary.BigEndian.Uint16(packet[offset : offset+2]),
				Class: binary.BigEndian.Uint16(packet[offset+2 : offset+4]),
				End:   offset + 4,
			}, nil
		}
		if length&0xc0 != 0 {
			return Question{}, ErrCompressedQNAME
		}
		if length > 63 || offset+length > len(packet) {
			return Question{}, ErrInvalidLabel
		}
		label := packet[offset : offset+length]
		for _, b := range label {
			if b == 0 || b == '.' {
				return Question{}, ErrInvalidLabel
			}
		}
		labels = append(labels, string(label))
		offset += length
	}
}

// NormalizeDomain canonicalizes a configured route suffix.
func NormalizeDomain(domain string) (string, error) {
	domain = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(domain, ".")))
	if domain == "" || len(domain) > 253 {
		return "", fmt.Errorf("invalid domain %q", domain)
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || len(label) > 63 {
			return "", fmt.Errorf("invalid domain %q", domain)
		}
	}
	return domain, nil
}

// RefusedResponse builds a minimal REFUSED reply while retaining the original
// question, transaction ID, recursion-desired bit, and any EDNS data.
func RefusedResponse(query []byte) []byte {
	return ErrorResponse(query, 5)
}

// ErrorResponse builds a DNS error reply with the requested four-bit RCODE.
func ErrorResponse(query []byte, rcode uint16) []byte {
	if len(query) < 12 {
		return nil
	}
	response := append([]byte(nil), query...)
	flags := binary.BigEndian.Uint16(response[2:4])
	flags &^= 0x040f // clear AA and existing RCODE
	flags |= 0x8000 | (rcode & 0x000f)
	binary.BigEndian.PutUint16(response[2:4], flags)
	return response
}
